package probe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

type pipeListener struct {
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{
		conns:  make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-l.conns:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
	})
	return nil
}

type dummyAddr struct{}

func (dummyAddr) Network() string { return "pipe" }
func (dummyAddr) String() string  { return "pipe" }

func (l *pipeListener) Addr() net.Addr {
	return dummyAddr{}
}

func newLocalWSPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	listener := newPipeListener()

	serverWSChan := make(chan *websocket.Conn, 1)
	serverErrChan := make(chan error, 1)

	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				serverErrChan <- err
				return
			}
			serverWSChan <- conn
		}),
	}

	go func() {
		_ = server.Serve(listener)
	}()

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				c1, c2 := net.Pipe()
				select {
				case listener.conns <- c2:
					return c1, nil
				case <-listener.closed:
					_ = c1.Close()
					_ = c2.Close()
					return nil, net.ErrClosed
				case <-ctx.Done():
					_ = c1.Close()
					_ = c2.Close()
					return nil, ctx.Err()
				}
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clientConn, _, err := websocket.Dial(ctx, "http://localhost/ws", &websocket.DialOptions{
		HTTPClient: httpClient,
	})
	if err != nil {
		t.Fatalf("websocket.Dial failed: %v", err)
	}

	var serverConn *websocket.Conn
	select {
	case serverConn = <-serverWSChan:
	case err := <-serverErrChan:
		t.Fatalf("websocket.Accept failed: %v", err)
	case <-ctx.Done():
		t.Fatal("timeout waiting for server connection")
	}

	t.Cleanup(func() {
		_ = clientConn.CloseNow()
		_ = serverConn.CloseNow()
		_ = listener.Close()
		_ = server.Close()
	})

	return clientConn, serverConn
}

func makeValidControlFrame(t *testing.T, gen probe.Decimal, msgType string) []byte {
	t.Helper()
	env := map[string]any{
		"protocol_version":      1,
		"type":                  msgType,
		"message_id":            uuid.New().String(),
		"sent_at":               time.Now().UTC().Format(time.RFC3339Nano),
		"connection_generation": fmt.Sprintf("%d", gen),
		"payload": map[string]any{
			"dummy": "value",
		},
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeValidHealthFrame(t *testing.T, gen probe.Decimal, role string) []byte {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := map[string]any{
		"role":              role,
		"ready":             true,
		"db_writable":       true,
		"config_revision":   "1",
		"committed_seq":     "10",
		"clock_time":        now,
		"errors":            []string{},
		"scheduler_healthy": nil,
		"queue_bytes":       nil,
		"oldest_queued_at":  nil,
	}
	if role == "probe" {
		sched := true
		payload["scheduler_healthy"] = &sched
		qBytes := int64(0)
		payload["queue_bytes"] = &qBytes
	}

	env := map[string]any{
		"protocol_version":      1,
		"type":                  "health",
		"message_id":            uuid.New().String(),
		"sent_at":               now,
		"connection_generation": fmt.Sprintf("%d", gen),
		"payload":               payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeValidTelemetryGapFrame(t *testing.T, gen probe.Decimal) []byte {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload := map[string]any{
		"stream_id":            uuid.New().String(),
		"from_seq":             "1",
		"through_seq":          "5",
		"reason":               "disk_pressure",
		"observed_from":        now,
		"observed_through":     now,
		"affected_monitor_ids": []int64{101},
	}
	env := map[string]any{
		"protocol_version":      1,
		"type":                  "telemetry.gap",
		"message_id":            uuid.New().String(),
		"sent_at":               now,
		"connection_generation": fmt.Sprintf("%d", gen),
		"payload":               payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func makeValidTelemetryBatchFrame(t *testing.T, gen probe.Decimal) []byte {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	event := map[string]any{
		"seq":         "1",
		"kind":        "observation",
		"observed_at": now,
		"data": map[string]any{
			"monitor_id":            1,
			"assignment_generation": "1",
			"config_revision":       "1",
			"status":                "UP",
			"raw_status":            "UP",
			"down_count":            0,
			"ping":                  20,
			"duration_ms":           50,
			"message":               "ok",
			"important":             false,
			"conditions":            []any{},
			"tls":                   nil,
		},
	}
	payload := map[string]any{
		"stream_id": uuid.New().String(),
		"first_seq": "1",
		"last_seq":  "1",
		"events":    []any{event},
	}
	env := map[string]any{
		"protocol_version":      1,
		"type":                  "telemetry.batch",
		"message_id":            uuid.New().String(),
		"sent_at":               now,
		"connection_generation": fmt.Sprintf("%d", gen),
		"payload":               payload,
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestSession_NewSessionValidation(t *testing.T) {
	clientConn, _ := newLocalWSPair(t)

	// nil conn
	_, err := probe.NewSession(nil, probe.SessionConfig{Generation: 1, PeerRole: "hub"})
	if err == nil || !strings.Contains(err.Error(), "connection is required") {
		t.Fatalf("expected connection required error, got %v", err)
	}

	// generation <= 0
	_, err = probe.NewSession(clientConn, probe.SessionConfig{Generation: 0, PeerRole: "hub"})
	if err == nil || !strings.Contains(err.Error(), "generation must be positive") {
		t.Fatalf("expected generation must be positive error, got %v", err)
	}

	// invalid peer role
	_, err = probe.NewSession(clientConn, probe.SessionConfig{Generation: 1, PeerRole: "admin"})
	if err == nil || !strings.Contains(err.Error(), "peer role must be 'hub' or 'probe'") {
		t.Fatalf("expected peer role error, got %v", err)
	}

	// valid config
	sess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: 1, PeerRole: "probe"})
	if err != nil {
		t.Fatalf("unexpected error for valid config: %v", err)
	}
	_ = sess.Close()
}

func TestSession_SingleRun(t *testing.T) {
	clientConn, _ := newLocalWSPair(t)
	sess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: 1, PeerRole: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sess.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- sess.Run(ctx, func(c context.Context, e probe.Envelope) error {
			return nil
		})
	}()

	time.Sleep(20 * time.Millisecond)

	// Concurrent Run call must fail immediately
	secondErr := sess.Run(ctx, func(c context.Context, e probe.Envelope) error {
		return nil
	})
	if secondErr == nil || !strings.Contains(secondErr.Error(), "single-use") {
		t.Fatalf("expected single-use error, got %v", secondErr)
	}

	cancel()
	<-runErrCh

	// Sequential Run call after finished must also fail
	thirdErr := sess.Run(context.Background(), func(c context.Context, e probe.Envelope) error {
		return nil
	})
	if thirdErr == nil || !strings.Contains(thirdErr.Error(), "single-use") {
		t.Fatalf("expected single-use error on subsequent run, got %v", thirdErr)
	}
}

func TestSession_ConcurrentRunAndClose(t *testing.T) {
	for i := 0; i < 20; i++ {
		clientConn, _ := newLocalWSPair(t)
		sess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: 1, PeerRole: "probe"})
		if err != nil {
			t.Fatal(err)
		}

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			_ = sess.Run(context.Background(), func(c context.Context, e probe.Envelope) error {
				return nil
			})
		}()

		go func() {
			defer wg.Done()
			_ = sess.Close()
		}()

		wg.Wait()
	}
}

func TestSession_CloseBeforeRun(t *testing.T) {
	for i := 0; i < 10; i++ {
		clientConn, _ := newLocalWSPair(t)
		sess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: 1, PeerRole: "probe"})
		if err != nil {
			t.Fatal(err)
		}

		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}

		err = sess.Run(context.Background(), func(c context.Context, e probe.Envelope) error {
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "closed") {
			t.Fatalf("expected session closed error when Run called after Close, got %v", err)
		}
	}
}

func TestSession_SendControlAndReceive(t *testing.T) {
	clientConn, serverConn := newLocalWSPair(t)

	gen := probe.Decimal(42)
	clientSess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	serverSess, err := probe.NewSession(serverConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan probe.Envelope, 1)
	go func() {
		_ = serverSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			received <- env
			return nil
		})
	}()
	go func() {
		_ = clientSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			return nil
		})
	}()

	controlFrame := makeValidControlFrame(t, gen, "config.begin")
	if err := clientSess.SendControl(ctx, controlFrame); err != nil {
		t.Fatalf("SendControl failed: %v", err)
	}

	select {
	case env := <-received:
		if env.Type != "config.begin" {
			t.Fatalf("expected config.begin, got %s", env.Type)
		}
		if env.ConnectionGeneration != gen {
			t.Fatalf("expected gen %d, got %d", gen, env.ConnectionGeneration)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for control frame")
	}
}

func TestSession_SendReplayAndReceive(t *testing.T) {
	clientConn, serverConn := newLocalWSPair(t)

	gen := probe.Decimal(10)
	clientSess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	serverSess, err := probe.NewSession(serverConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan probe.Envelope, 2)
	go func() {
		_ = serverSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			received <- env
			return nil
		})
	}()
	go func() {
		_ = clientSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			return nil
		})
	}()

	// 1. Send telemetry.gap
	gapFrame := makeValidTelemetryGapFrame(t, gen)
	if err := clientSess.SendReplay(ctx, gapFrame); err != nil {
		t.Fatalf("SendReplay (gap) failed: %v", err)
	}

	// 2. Send telemetry.batch
	batchFrame := makeValidTelemetryBatchFrame(t, gen)
	if err := clientSess.SendReplay(ctx, batchFrame); err != nil {
		t.Fatalf("SendReplay (batch) failed: %v", err)
	}

	select {
	case env := <-received:
		if env.Type != "telemetry.gap" {
			t.Fatalf("expected telemetry.gap, got %s", env.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for telemetry.gap")
	}

	select {
	case env := <-received:
		if env.Type != "telemetry.batch" {
			t.Fatalf("expected telemetry.batch, got %s", env.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for telemetry.batch")
	}
}

func TestSession_ValidationFailures(t *testing.T) {
	clientConn, _ := newLocalWSPair(t)
	gen := probe.Decimal(5)
	sess, err := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = sess.Close()
	}()

	ctx := context.Background()

	t.Run("frame exceeds max bytes", func(t *testing.T) {
		oversized := make([]byte, probe.MaxFrameBytes+1)
		err := sess.SendControl(ctx, oversized)
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum bytes") {
			t.Fatalf("expected frame exceeds maximum bytes error, got %v", err)
		}
	})

	t.Run("malformed json redacted", func(t *testing.T) {
		sentinelSecret := "CONFIDENTIAL_PAYLOAD_VALUE_DO_NOT_ECHO"
		badJSON := fmt.Sprintf(`{"protocol_version": 1, "type": "config.begin", "connection_generation": "%s"`, sentinelSecret)
		err := sess.SendControl(ctx, []byte(badJSON))
		if err == nil || !strings.Contains(err.Error(), "invalid control frame") {
			t.Fatalf("expected invalid control frame error, got %v", err)
		}
		if strings.Contains(err.Error(), sentinelSecret) {
			t.Fatalf("error leaked confidential payload value: %v", err)
		}
	})

	t.Run("generation mismatch", func(t *testing.T) {
		mismatchedFrame := makeValidControlFrame(t, gen+1, "config.begin")
		err := sess.SendControl(ctx, mismatchedFrame)
		if err == nil || !strings.Contains(err.Error(), "generation mismatch") {
			t.Fatalf("expected generation mismatch error, got %v", err)
		}
	})

	t.Run("handshake frame forbidden", func(t *testing.T) {
		for _, hs := range []string{"hello", "welcome", "enroll.request", "enroll.result"} {
			genForFrame := probe.Decimal(0)
			if hs == "welcome" {
				genForFrame = gen
			}
			frame := makeValidControlFrame(t, genForFrame, hs)
			err := sess.SendControl(ctx, frame)
			if err == nil || !strings.Contains(err.Error(), "forbidden during established session") {
				t.Fatalf("expected handshake forbidden error for %s, got %v", hs, err)
			}
		}
	})

	t.Run("control rejects replay types", func(t *testing.T) {
		gap := makeValidTelemetryGapFrame(t, gen)
		err := sess.SendControl(ctx, gap)
		if err == nil || !strings.Contains(err.Error(), "control must reject replay types") {
			t.Fatalf("expected control rejects replay types error, got %v", err)
		}
	})

	t.Run("replay rejects non-replay types", func(t *testing.T) {
		ctrl := makeValidControlFrame(t, gen, "config.begin")
		err := sess.SendReplay(ctx, ctrl)
		if err == nil || !strings.Contains(err.Error(), "replay accepts only telemetry.batch and telemetry.gap") {
			t.Fatalf("expected replay accepts only error, got %v", err)
		}
	})
}

func TestSession_IncomingFrameRejections(t *testing.T) {
	gen := probe.Decimal(1)

	t.Run("generation mismatch closes session", func(t *testing.T) {
		clientConn, serverConn := newLocalWSPair(t)
		clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
		defer func() { _ = clientSess.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- clientSess.Run(ctx, func(c context.Context, e probe.Envelope) error {
				return nil
			})
		}()

		// Send frame with generation 99 directly via serverConn
		mismatched := makeValidControlFrame(t, 99, "config.begin")
		_ = serverConn.Write(ctx, websocket.MessageText, mismatched)

		err := <-runErrCh
		if err == nil || !strings.Contains(err.Error(), "generation mismatch") {
			t.Fatalf("expected generation mismatch error, got %v", err)
		}
	})

	t.Run("health role mismatch closes session", func(t *testing.T) {
		clientConn, serverConn := newLocalWSPair(t)
		clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
		defer func() { _ = clientSess.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- clientSess.Run(ctx, func(c context.Context, e probe.Envelope) error {
				return nil
			})
		}()

		// Send health frame with role "hub" instead of "probe"
		wrongRoleHealth := makeValidHealthFrame(t, gen, "hub")
		_ = serverConn.Write(ctx, websocket.MessageText, wrongRoleHealth)

		err := <-runErrCh
		if err == nil || !strings.Contains(err.Error(), "health peer role mismatch") {
			t.Fatalf("expected health peer role mismatch error, got %v", err)
		}
	})

	t.Run("binary frame rejected", func(t *testing.T) {
		clientConn, serverConn := newLocalWSPair(t)
		clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
		defer func() { _ = clientSess.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- clientSess.Run(ctx, func(c context.Context, e probe.Envelope) error {
				return nil
			})
		}()

		_ = serverConn.Write(ctx, websocket.MessageBinary, []byte("binary data"))

		err := <-runErrCh
		if err == nil || !strings.Contains(err.Error(), "binary frames are not allowed") {
			t.Fatalf("expected binary frames error, got %v", err)
		}
	})

	t.Run("peer close reason redacted", func(t *testing.T) {
		clientConn, serverConn := newLocalWSPair(t)
		clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
		defer func() { _ = clientSess.Close() }()

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- clientSess.Run(ctx, func(c context.Context, e probe.Envelope) error {
				return nil
			})
		}()

		sentinelSecret := "ATTACKER_SECRET_REASON_STRING"
		_ = serverConn.Close(websocket.StatusNormalClosure, sentinelSecret)

		err := <-runErrCh
		if err == nil {
			t.Fatal("expected error on peer close, got nil")
		}
		if strings.Contains(err.Error(), sentinelSecret) {
			t.Fatalf("error leaked peer close reason: %v", err)
		}
	})
}

func TestSession_ByteSliceOwnership(t *testing.T) {
	clientConn, serverConn := newLocalWSPair(t)
	gen := probe.Decimal(1)
	clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	serverSess, _ := probe.NewSession(serverConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
	defer func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	received := make(chan probe.Envelope, 1)
	go func() {
		_ = serverSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			received <- env
			return nil
		})
	}()
	go func() {
		_ = clientSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			return nil
		})
	}()

	frame := makeValidControlFrame(t, gen, "config.begin")
	originalFrame := bytes.Clone(frame)

	if err := clientSess.SendControl(ctx, frame); err != nil {
		t.Fatalf("SendControl failed: %v", err)
	}

	// Mutate caller's slice immediately after SendControl completes
	for i := range frame {
		frame[i] = 'X'
	}

	select {
	case env := <-received:
		origEnv, _ := probe.DecodeEnvelope(originalFrame)
		if env.MessageID != origEnv.MessageID {
			t.Fatalf("received frame was mutated! got %s, want %s", env.MessageID, origEnv.MessageID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for frame")
	}
}

func TestSession_CallerContextCancellationWhileQueued(t *testing.T) {
	clientConn, _ := newLocalWSPair(t)
	gen := probe.Decimal(1)
	sess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	defer func() {
		_ = sess.Close()
	}()

	frame := makeValidControlFrame(t, gen, "config.begin")

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sess.SendControl(cancelCtx, frame)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSession_CloseUnblocksPendingSends(t *testing.T) {
	clientConn, _ := newLocalWSPair(t)
	gen := probe.Decimal(1)
	sess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame := makeValidControlFrame(t, gen, "config.begin")

	sendErrCh := make(chan error, 1)
	go func() {
		sendErrCh <- sess.SendControl(ctx, frame)
	}()

	time.Sleep(20 * time.Millisecond)

	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-sendErrCh:
		if err == nil || !strings.Contains(err.Error(), "session") {
			t.Fatalf("expected session closed error, got %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("SendControl did not unblock on session Close")
	}

	if err := sess.Close(); err != nil {
		t.Fatalf("idempotent Close failed: %v", err)
	}
}

func TestSession_FairnessAndPriority(t *testing.T) {
	clientConn, serverConn := newLocalWSPair(t)
	gen := probe.Decimal(1)
	clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	serverSess, _ := probe.NewSession(serverConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
	defer func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var receivedMu sync.Mutex
	var receivedOrder []string

	allReceived := make(chan struct{})
	totalExpected := 10 // 8 control + 2 replay

	go func() {
		_ = serverSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			receivedMu.Lock()
			receivedOrder = append(receivedOrder, env.Type)
			count := len(receivedOrder)
			receivedMu.Unlock()
			if count == totalExpected {
				close(allReceived)
			}
			return nil
		})
	}()

	go func() {
		_ = clientSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			return nil
		})
	}()

	var sendWg sync.WaitGroup
	for i := 0; i < 8; i++ {
		sendWg.Add(1)
		go func() {
			defer sendWg.Done()
			f := makeValidControlFrame(t, gen, "config.begin")
			_ = clientSess.SendControl(ctx, f)
		}()
	}
	for i := 0; i < 2; i++ {
		sendWg.Add(1)
		go func() {
			defer sendWg.Done()
			f := makeValidTelemetryGapFrame(t, gen)
			_ = clientSess.SendReplay(ctx, f)
		}()
	}

	sendWg.Wait()

	select {
	case <-allReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for all frames")
	}

	receivedMu.Lock()
	defer receivedMu.Unlock()

	if len(receivedOrder) != totalExpected {
		t.Fatalf("expected %d frames, got %d", totalExpected, len(receivedOrder))
	}

	replayCount := 0
	controlCount := 0
	for _, typ := range receivedOrder {
		if typ == "telemetry.gap" {
			replayCount++
		} else {
			controlCount++
		}
	}
	if replayCount != 2 || controlCount != 8 {
		t.Fatalf("expected 2 replay and 8 control frames, got %d replay and %d control", replayCount, controlCount)
	}
}

func TestSession_InFlightCancellationTerminatesSocketAndSiblings(t *testing.T) {
	clientConn, serverConn := newLocalWSPair(t)
	gen := probe.Decimal(1)
	clientSess, _ := probe.NewSession(clientConn, probe.SessionConfig{Generation: gen, PeerRole: "hub"})
	serverSess, _ := probe.NewSession(serverConn, probe.SessionConfig{Generation: gen, PeerRole: "probe"})
	defer func() {
		_ = clientSess.Close()
		_ = serverSess.Close()
	}()

	ctx := context.Background()

	// Server does NOT read from serverConn, causing socket write backpressure
	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- clientSess.Run(ctx, func(c context.Context, env probe.Envelope) error {
			return nil
		})
	}()

	inFlightCtx, cancelInFlight := context.WithCancel(context.Background())

	// Sibling send that waits in queue
	siblingErrCh := make(chan error, 1)
	inFlightErrCh := make(chan error, 1)

	frame := makeValidControlFrame(t, gen, "config.begin")

	// Launch in-flight write
	go func() {
		inFlightErrCh <- clientSess.SendControl(inFlightCtx, frame)
	}()

	time.Sleep(10 * time.Millisecond)

	// Launch sibling send
	go func() {
		siblingErrCh <- clientSess.SendControl(ctx, frame)
	}()

	time.Sleep(10 * time.Millisecond)

	// Cancel the in-flight caller
	cancelInFlight()

	select {
	case err := <-inFlightErrCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected inFlight context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for in-flight sender to return")
	}

	// Sibling send must also terminate promptly because socket was closed
	select {
	case err := <-siblingErrCh:
		if err == nil {
			t.Fatal("expected sibling send error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for sibling sender to terminate")
	}
}

func TestReconnectDelay(t *testing.T) {
	tests := []struct {
		name       string
		failures   int
		healthyFor time.Duration
		random     float64
		wantMin    time.Duration
		wantMax    time.Duration
	}{
		{
			name:       "failure 1 zero jitter",
			failures:   1,
			healthyFor: 0,
			random:     0.0,
			wantMin:    1 * time.Second, // 1s * 0.75 = 750ms -> clamped to 1s
			wantMax:    1 * time.Second,
		},
		{
			name:       "failure 1 max jitter",
			failures:   1,
			healthyFor: 0,
			random:     1.0,
			wantMin:    1250 * time.Millisecond,
			wantMax:    1250 * time.Millisecond,
		},
		{
			name:       "failure 2 mid jitter",
			failures:   2,
			healthyFor: 0,
			random:     0.5,
			wantMin:    2 * time.Second, // 2s * 1.0 = 2s
			wantMax:    2 * time.Second,
		},
		{
			name:       "failure 3 mid jitter",
			failures:   3,
			healthyFor: 0,
			random:     0.5,
			wantMin:    4 * time.Second, // 4s * 1.0 = 4s
			wantMax:    4 * time.Second,
		},
		{
			name:       "failure 4 mid jitter",
			failures:   4,
			healthyFor: 0,
			random:     0.5,
			wantMin:    8 * time.Second, // 8s * 1.0 = 8s
			wantMax:    8 * time.Second,
		},
		{
			name:       "failure 5 mid jitter",
			failures:   5,
			healthyFor: 0,
			random:     0.5,
			wantMin:    16 * time.Second, // 16s * 1.0 = 16s
			wantMax:    16 * time.Second,
		},
		{
			name:       "failure 6 mid jitter",
			failures:   6,
			healthyFor: 0,
			random:     0.5,
			wantMin:    30 * time.Second, // 30s * 1.0 = 30s
			wantMax:    30 * time.Second,
		},
		{
			name:       "large failure clamped to 30s",
			failures:   100,
			healthyFor: 0,
			random:     1.0,
			wantMin:    30 * time.Second, // 30s * 1.25 = 37.5s -> clamped to 30s
			wantMax:    30 * time.Second,
		},
		{
			name:       "negative failure treated as 1",
			failures:   -5,
			healthyFor: 0,
			random:     0.5,
			wantMin:    1 * time.Second,
			wantMax:    1 * time.Second,
		},
		{
			name:       "healthy >= 30s resets to failure 1",
			failures:   5,
			healthyFor: 35 * time.Second,
			random:     0.5,
			wantMin:    1 * time.Second,
			wantMax:    1 * time.Second,
		},
		{
			name:       "healthy < 30s does not reset",
			failures:   5,
			healthyFor: 25 * time.Second,
			random:     0.5,
			wantMin:    16 * time.Second,
			wantMax:    16 * time.Second,
		},
		{
			name:       "random < 0 clamped to 0",
			failures:   2,
			healthyFor: 0,
			random:     -0.5,
			wantMin:    1500 * time.Millisecond, // 2s * 0.75 = 1.5s
			wantMax:    1500 * time.Millisecond,
		},
		{
			name:       "random > 1 clamped to 1",
			failures:   2,
			healthyFor: 0,
			random:     1.5,
			wantMin:    2500 * time.Millisecond, // 2s * 1.25 = 2.5s
			wantMax:    2500 * time.Millisecond,
		},
		{
			name:       "random NaN clamped to 1",
			failures:   2,
			healthyFor: 0,
			random:     math.NaN(),
			wantMin:    2500 * time.Millisecond,
			wantMax:    2500 * time.Millisecond,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := probe.ReconnectDelay(tc.failures, tc.healthyFor, tc.random)
			if got < tc.wantMin || got > tc.wantMax {
				t.Fatalf("ReconnectDelay(%d, %v, %f) = %v, want in [%v, %v]",
					tc.failures, tc.healthyFor, tc.random, got, tc.wantMin, tc.wantMax)
			}
		})
	}
}
