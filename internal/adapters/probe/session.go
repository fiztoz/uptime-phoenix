package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// SessionConfig configures an established runtime session between a hub and a probe.
type SessionConfig struct {
	Generation Decimal // positive; negotiation has already completed
	PeerRole   string  // "hub" or "probe", for health sender validation
}

type sendItem struct {
	ctx  context.Context
	data []byte
	done chan error
}

// Session manages the bidirectional transport over an established WebSocket connection.
type Session struct {
	conn           *websocket.Conn
	cfg            SessionConfig
	mu             sync.Mutex
	closed         chan struct{}
	closeErr       error
	isClosed       bool
	runStarted     int32
	sessCtx        context.Context
	cancelSess     context.CancelFunc
	controlQueue   chan *sendItem
	replayQueue    chan *sendItem
	readTimeout    time.Duration
	writeTimeout   time.Duration
	handlerTimeout time.Duration
}

// NewSession constructs a new Session over an already-negotiated WebSocket connection.
func NewSession(conn *websocket.Conn, cfg SessionConfig) (*Session, error) {
	if conn == nil {
		return nil, errors.New("websocket connection is required")
	}
	if cfg.Generation <= 0 {
		return nil, errors.New("session generation must be positive")
	}
	if cfg.PeerRole != "hub" && cfg.PeerRole != "probe" {
		return nil, errors.New("peer role must be 'hub' or 'probe'")
	}

	conn.SetReadLimit(MaxFrameBytes)

	sessCtx, cancelSess := context.WithCancel(context.Background())

	return &Session{
		conn:           conn,
		cfg:            cfg,
		closed:         make(chan struct{}),
		sessCtx:        sessCtx,
		cancelSess:     cancelSess,
		controlQueue:   make(chan *sendItem, 16),
		replayQueue:    make(chan *sendItem, 16),
		readTimeout:    45 * time.Second,
		writeTimeout:   10 * time.Second,
		handlerTimeout: 10 * time.Second,
	}, nil
}

// Run executes the reader loop and writer loop. It is single-use and blocks until
// the session closes, the context is canceled, or an unrecoverable error occurs.
func (s *Session) Run(ctx context.Context, handle func(context.Context, Envelope) error) error {
	if handle == nil {
		return errors.New("handler function is required")
	}
	return s.run(ctx, func(ctx context.Context, e Envelope, _ time.Time, _ *Health) error { return handle(ctx, e) })
}

func (s *Session) run(ctx context.Context, handle func(context.Context, Envelope, time.Time, *Health) error) error {
	return s.runAdmission(ctx, handle, nil)
}

func (s *Session) runAdmission(ctx context.Context, handle func(context.Context, Envelope, time.Time, *Health) error, admission ports.ProbeHealthAdmission) error {
	if !atomic.CompareAndSwapInt32(&s.runStarted, 0, 1) {
		return errors.New("session Run is single-use and cannot be called concurrently or repeated")
	}

	s.mu.Lock()
	if s.isClosed {
		s.mu.Unlock()
		return errors.New("session is closed")
	}
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Link session cancellation (from Close) to runCtx without a race
	stop := context.AfterFunc(s.sessCtx, cancel)
	defer stop()

	defer func() {
		_ = s.Close()
	}()

	writerErrCh := make(chan error, 1)
	go func() {
		writerErrCh <- s.writerLoop(runCtx)
	}()

	readerErr := s.readerLoop(runCtx, handle, admission)

	cancel()
	writerErr := <-writerErrCh

	if readerErr != nil && !errors.Is(readerErr, context.Canceled) {
		return readerErr
	}
	if writerErr != nil && !errors.Is(writerErr, context.Canceled) {
		return writerErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// SendControl sends a control-plane frame (such as health, config, or ack).
// It blocks until the frame is actually written to the wire or an error occurs.
func (s *Session) SendControl(ctx context.Context, frame []byte) error {
	if err := s.checkSendState(ctx); err != nil {
		return err
	}
	if len(frame) > MaxFrameBytes {
		return errors.New("frame exceeds maximum bytes")
	}

	cloned := bytes.Clone(frame)
	envelope, err := DecodeEnvelope(cloned)
	if err != nil {
		return errors.New("invalid control frame")
	}
	if isHandshakeOrEnrollment(envelope.Type) {
		return errors.New("handshake and enrollment frames are forbidden during established session")
	}
	if envelope.ConnectionGeneration != s.cfg.Generation {
		return errors.New("connection generation mismatch")
	}
	if envelope.Type == "telemetry.batch" || envelope.Type == "telemetry.gap" {
		return errors.New("control must reject replay types")
	}

	item := &sendItem{
		ctx:  ctx,
		data: cloned,
		done: make(chan error, 1),
	}

	select {
	case <-s.closed:
		return errors.New("session is closed")
	case <-ctx.Done():
		return ctx.Err()
	case s.controlQueue <- item:
	}

	select {
	case <-s.closed:
		return errors.New("session is closed")
	case <-ctx.Done():
		return ctx.Err()
	case err := <-item.done:
		return err
	}
}

// SendReplay sends a replay-plane frame (telemetry.batch or telemetry.gap).
// It blocks until the frame is actually written to the wire or an error occurs.
func (s *Session) SendReplay(ctx context.Context, frame []byte) error {
	if err := s.checkSendState(ctx); err != nil {
		return err
	}
	if len(frame) > MaxFrameBytes {
		return errors.New("frame exceeds maximum bytes")
	}

	cloned := bytes.Clone(frame)
	envelope, err := DecodeEnvelope(cloned)
	if err != nil {
		return errors.New("invalid replay frame")
	}
	if isHandshakeOrEnrollment(envelope.Type) {
		return errors.New("handshake and enrollment frames are forbidden during established session")
	}
	if envelope.ConnectionGeneration != s.cfg.Generation {
		return errors.New("connection generation mismatch")
	}

	switch envelope.Type {
	case "telemetry.batch":
		if _, _, err := DecodeTelemetryBatch(cloned); err != nil {
			return errors.New("invalid telemetry batch")
		}
	case "telemetry.gap":
		if _, _, err := DecodeTelemetryGap(cloned); err != nil {
			return errors.New("invalid telemetry gap")
		}
	default:
		return errors.New("replay accepts only telemetry.batch and telemetry.gap frames")
	}

	item := &sendItem{
		ctx:  ctx,
		data: cloned,
		done: make(chan error, 1),
	}

	select {
	case <-s.closed:
		return errors.New("session is closed")
	case <-ctx.Done():
		return ctx.Err()
	case s.replayQueue <- item:
	}

	select {
	case <-s.closed:
		return errors.New("session is closed")
	case <-ctx.Done():
		return ctx.Err()
	case err := <-item.done:
		return err
	}
}

// Close closes the session and the underlying websocket connection with CloseNow.
// It is idempotent and concurrency-safe.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isClosed {
		return s.closeErr
	}
	s.isClosed = true
	close(s.closed)
	s.cancelSess()
	if s.conn != nil {
		s.closeErr = s.conn.CloseNow()
	}
	return s.closeErr
}

func (s *Session) checkSendState(ctx context.Context) error {
	select {
	case <-s.closed:
		return errors.New("session is closed")
	default:
	}
	return ctx.Err()
}

func (s *Session) readerLoop(ctx context.Context, handle func(context.Context, Envelope, time.Time, *Health) error, admission ports.ProbeHealthAdmission) error {
	for {
		readTimeout := s.readTimeout
		if readTimeout <= 0 {
			readTimeout = 45 * time.Second
		}
		readCtx, readCancel := context.WithTimeout(ctx, readTimeout)
		msgType, data, err := s.conn.Read(readCtx)
		receivedAt := time.Now() // Preserve the process monotonic component before dispatch.
		readCancel()

		if err != nil {
			_ = s.Close()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			return errors.New("websocket read failed")
		}

		var envelope Envelope
		var health *Health
		decode := func(at time.Time) (*bool, error) {
			receivedAt = at
			envelope, health, err = s.decodeIncoming(msgType, data)
			if err != nil || health == nil {
				return nil, err
			}
			healthy := health.Ready && health.DBWritable && (health.Role == "hub" || health.SchedulerHealthy != nil && *health.SchedulerHealthy)
			return &healthy, nil
		}
		if admission != nil {
			// Clock capture and the watchdog's tick reservation share this
			// bounded validation gate. Close and all worker callbacks stay outside.
			err = admission.Admit(int64(s.cfg.Generation), decode)
		} else {
			_, err = decode(receivedAt)
		}
		if err != nil {
			_ = s.Close()
			return err
		}

		handlerTimeout := s.handlerTimeout
		if handlerTimeout <= 0 {
			handlerTimeout = 10 * time.Second
		}
		handleCtx, handleCancel := context.WithTimeout(ctx, handlerTimeout)
		err = handle(handleCtx, envelope, receivedAt, health)
		handleCancel()

		if err != nil {
			_ = s.Close()
			return fmt.Errorf("handler failed: %w", err)
		}
	}
}

func (s *Session) decodeIncoming(msgType websocket.MessageType, data []byte) (Envelope, *Health, error) {
	if msgType != websocket.MessageText {
		return Envelope{}, nil, errors.New("binary frames are not allowed")
	}
	envelope, err := DecodeEnvelope(data)
	if err != nil {
		return Envelope{}, nil, errors.New("invalid incoming frame")
	}
	if envelope.ConnectionGeneration != s.cfg.Generation {
		return Envelope{}, nil, errors.New("incoming frame connection generation mismatch")
	}
	if isHandshakeOrEnrollment(envelope.Type) {
		return Envelope{}, nil, errors.New("handshake and enrollment frames are forbidden during established session")
	}
	if envelope.Type != "health" {
		return envelope, nil, nil
	}
	_, health, err := DecodeHealth(data)
	if err != nil {
		return Envelope{}, nil, errors.New("invalid health frame payload")
	}
	if health.Role != s.cfg.PeerRole {
		return Envelope{}, nil, errors.New("health peer role mismatch")
	}
	return envelope, &health, nil
}

func (s *Session) writerLoop(ctx context.Context) error {
	consecutiveControl := 0
	defer s.drainQueues(errors.New("session closed"))

	for {
		item, err := s.nextItem(ctx, &consecutiveControl)
		if err != nil {
			return err
		}

		// Check caller context at the actual write boundary
		if err := item.ctx.Err(); err != nil {
			item.done <- err
			continue
		}

		writeTimeout := s.writeTimeout
		if writeTimeout <= 0 {
			writeTimeout = 10 * time.Second
		}
		writeCtx, writeCancel := context.WithTimeout(ctx, writeTimeout)

		// Connect caller's context cancellation using context.AfterFunc without extra goroutines
		stop := context.AfterFunc(item.ctx, func() {
			writeCancel()
		})

		err = s.conn.Write(writeCtx, websocket.MessageText, item.data)
		stop()
		writeCancel()

		if err != nil {
			// An interrupted or failed write closes the session because partial frame delivery is uncertain.
			_ = s.Close()
			if item.ctx.Err() != nil {
				item.done <- item.ctx.Err()
			} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				item.done <- err
			} else {
				item.done <- errors.New("websocket write failed")
			}
			return errors.New("websocket write failed")
		}

		item.done <- nil
	}
}

func (s *Session) nextItem(ctx context.Context, consecutiveControl *int) (*sendItem, error) {
	for {
		// Always check cancellation first before selecting from queues
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.closed:
			return nil, errors.New("session closed")
		default:
		}

		// Prioritize control, but after at most eight consecutive control frames write one waiting replay frame
		if *consecutiveControl >= 8 {
			select {
			case item := <-s.replayQueue:
				*consecutiveControl = 0
				return item, nil
			default:
				*consecutiveControl = 0
			}
		}

		select {
		case item := <-s.controlQueue:
			*consecutiveControl++
			return item, nil
		default:
		}

		select {
		case item := <-s.replayQueue:
			*consecutiveControl = 0
			return item, nil
		default:
		}

		// Both queues empty, wait for either or session cancellation
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.closed:
			return nil, errors.New("session closed")
		case item := <-s.controlQueue:
			*consecutiveControl++
			return item, nil
		case item := <-s.replayQueue:
			*consecutiveControl = 0
			return item, nil
		}
	}
}

func (s *Session) drainQueues(err error) {
	for {
		select {
		case item := <-s.controlQueue:
			item.done <- err
		case item := <-s.replayQueue:
			item.done <- err
		default:
			return
		}
	}
}

func isHandshakeOrEnrollment(kind string) bool {
	return kind == "hello" || kind == "welcome" || kind == "enroll.request" || kind == "enroll.result"
}

// ReconnectDelay calculates the bounded backoff delay before reconnecting.
// Base exponential: 1, 2, 4, 8, 16, 30 seconds for failures 1 onward.
// Jitter: within 75%–125%, clamped to [1s, 30s].
// Reset: resets to the first base (1s) only after at least 30 seconds of healthy operation.
func ReconnectDelay(failures int, healthyFor time.Duration, random float64) time.Duration {
	if healthyFor >= 30*time.Second {
		failures = 1
	}
	if failures < 1 {
		failures = 1
	}
	if failures > 6 {
		failures = 6
	}

	var base time.Duration
	switch failures {
	case 1:
		base = 1 * time.Second
	case 2:
		base = 2 * time.Second
	case 3:
		base = 4 * time.Second
	case 4:
		base = 8 * time.Second
	case 5:
		base = 16 * time.Second
	default:
		base = 30 * time.Second
	}

	if random < 0.0 {
		random = 0.0
	} else if random > 1.0 || math.IsNaN(random) {
		random = 1.0
	}

	jitter := 0.75 + 0.50*random
	delay := time.Duration(float64(base) * jitter)

	if delay < 1*time.Second {
		delay = 1 * time.Second
	} else if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return delay
}
