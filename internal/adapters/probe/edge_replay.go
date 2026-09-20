package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	// envelopeReservedBytes reserves headroom for the telemetry.batch framing.
	envelopeReservedBytes = 4096
	// replayIdleWait is the bounded idle sleep when the outbox has no pending rows.
	replayIdleWait = 250 * time.Millisecond
	// replayAckTimeout is the time before retransmitting an in-flight batch.
	replayAckTimeout = 5 * time.Second
	// replayMinBackoff is the minimum backoff delay after retry or timeout.
	replayMinBackoff = 100 * time.Millisecond
	// replayMaxBackoff is the maximum clamped backoff delay.
	replayMaxBackoff = 30 * time.Second
)

type inflightBatch struct {
	streamID  string
	firstSeq  int64
	lastSeq   int64
	itemCount int
	frame     []byte
	sent      bool
}

type completedBatchInfo struct {
	streamID     string
	committedSeq int64
	totalEvents  int64
	rejections   []domain.ProbeReplayRejection
}

type replayEvent struct {
	kind       string
	generation Decimal
	ack        TelemetryACK
	retry      TelemetryRetry
}

// edgeReplayPump manages the single session-owned replay loop that reads from
// the edge outbox and sends batches over Session.SendReplay.
type edgeReplayPump struct {
	repo               ports.EdgeReplayRepository
	session            *Session
	sendReplay         func(ctx context.Context, frame []byte) error
	hubID              string
	probeID            string
	streamID           string
	generation         Decimal
	mu                 sync.Mutex
	hubReady           bool
	statePending       bool
	readyCh            chan struct{}
	eventCh            chan replayEvent
	inflight           *inflightBatch
	lastCompletedBatch *completedBatchInfo
}

func newEdgeReplayPump(repo ports.EdgeReplayRepository, session *Session, hubID, probeID, streamID string, generation Decimal) *edgeReplayPump {
	p := &edgeReplayPump{
		repo:       repo,
		session:    session,
		hubID:      hubID,
		probeID:    probeID,
		streamID:   streamID,
		generation: generation,
		readyCh:    make(chan struct{}, 1),
		eventCh:    make(chan replayEvent, 16),
	}
	if session != nil {
		p.sendReplay = session.SendReplay
	}
	return p
}

func (p *edgeReplayPump) updateHubHealth(h Health) {
	ready := h.Role == "hub" && h.Ready && h.DBWritable && !slices.Contains(h.Errors, "ingest_unavailable")
	p.mu.Lock()
	p.hubReady = ready
	p.mu.Unlock()
	select {
	case p.readyCh <- struct{}{}:
	default:
	}
}

func (p *edgeReplayPump) isHubReady() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hubReady && !p.statePending
}

func (p *edgeReplayPump) setStatePending(pending bool) {
	p.mu.Lock()
	p.statePending = pending
	p.mu.Unlock()
	select {
	case p.readyCh <- struct{}{}:
	default:
	}
}

func (p *edgeReplayPump) isProvenDuplicateACK(ack TelemetryACK, gen Decimal) bool {
	p.mu.Lock()
	last := p.lastCompletedBatch
	p.mu.Unlock()
	if last == nil || gen != p.generation {
		return false
	}
	if ack.StreamID != last.streamID || int64(ack.CommittedSeq) != last.committedSeq {
		return false
	}
	totalOutcomes := ack.AcceptedCount + ack.DuplicateCount + int64(len(ack.Rejected))
	if totalOutcomes != last.totalEvents {
		return false
	}
	if len(ack.Rejected) != len(last.rejections) {
		return false
	}
	for i, r := range ack.Rejected {
		if int64(r.Seq) != last.rejections[i].Seq || r.Code != last.rejections[i].Code {
			return false
		}
	}
	return true
}

func (p *edgeReplayPump) handleACK(ctx context.Context, gen Decimal, ack TelemetryACK) error {
	ev := replayEvent{
		kind:       "telemetry.ack",
		generation: gen,
		ack:        ack,
	}
	select {
	case p.eventCh <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		if p.isProvenDuplicateACK(ack, gen) {
			return nil
		}
		if p.session != nil {
			_ = p.session.Close()
		}
		return errors.New("replay ack queue full")
	}
}

func (p *edgeReplayPump) handleRetry(ctx context.Context, gen Decimal, retry TelemetryRetry) error {
	ev := replayEvent{
		kind:       "telemetry.retry",
		generation: gen,
		retry:      retry,
	}
	select {
	case p.eventCh <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	default:
		if p.session != nil {
			_ = p.session.Close()
		}
		return errors.New("replay retry queue full")
	}
}

func (p *edgeReplayPump) run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if !p.isHubReady() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-p.readyCh:
				continue
			case ev := <-p.eventCh:
				if err := p.processEvent(ctx, nil, ev, nil); err != nil {
					if p.session != nil {
						_ = p.session.Close()
					}
					return err
				}
				continue
			case <-time.After(replayIdleWait):
				continue
			}
		}

		// Read starting at local cursor + 1 (fromSeq = 0).
		maxEvents := MaxBatchEvents
		maxBytes := MaxBatchBytes - envelopeReservedBytes
		batch, err := p.repo.ReadReplayBatch(ctx, 0, maxEvents, maxBytes)
		if err != nil {
			if p.session != nil {
				_ = p.session.Close()
			}
			return fmt.Errorf("read replay batch: %w", err)
		}

		if batch == nil || len(batch.Items) == 0 && batch.Gap == nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ev := <-p.eventCh:
				if err := p.processEvent(ctx, nil, ev, nil); err != nil {
					if p.session != nil {
						_ = p.session.Close()
					}
					return err
				}
				continue
			case <-time.After(replayIdleWait):
				continue
			}
		}

		// Validate exact event bytes against row seq, kind, and truncated microsecond time.
		for _, item := range batch.Items {
			if len(item.Payload) > MaxEventBytes {
				if p.session != nil {
					_ = p.session.Close()
				}
				return fmt.Errorf("event %d exceeds max bytes (%d > %d)", item.Seq, len(item.Payload), MaxEventBytes)
			}
			event, err := decodeTelemetryEvent(item.Payload)
			if err != nil {
				if p.session != nil {
					_ = p.session.Close()
				}
				return fmt.Errorf("decode event %d: %w", item.Seq, err)
			}
			if int64(event.Seq) != item.Seq {
				if p.session != nil {
					_ = p.session.Close()
				}
				return fmt.Errorf("event seq mismatch: expected %d, got %d", item.Seq, event.Seq)
			}
			if event.Kind != item.Kind {
				if p.session != nil {
					_ = p.session.Close()
				}
				return fmt.Errorf("event kind mismatch for seq %d: expected %s, got %s", item.Seq, item.Kind, event.Kind)
			}
			// Truncate payload time to microsecond integer for comparison with edge store observed_at.
			if time.Time(event.ObservedAt).UTC().UnixMicro() != item.ObservedAt.UTC().UnixMicro() {
				if p.session != nil {
					_ = p.session.Close()
				}
				return fmt.Errorf("event observed_at mismatch for seq %d: expected %d, got %d", item.Seq, item.ObservedAt.UTC().UnixMicro(), time.Time(event.ObservedAt).UTC().UnixMicro())
			}
		}

		var frameBytes []byte
		if batch.Gap != nil {
			frameBytes, err = buildReplayGapFrame(p.streamID, p.generation, batch)
		} else {
			frameBytes, err = buildReplayBatchFrame(p.streamID, p.generation, batch)
		}
		if err != nil {
			if p.session != nil {
				_ = p.session.Close()
			}
			return fmt.Errorf("build replay frame: %w", err)
		}

		inflight := &inflightBatch{
			streamID:  batch.StreamID,
			firstSeq:  batch.FirstSeq,
			lastSeq:   batch.LastSeq,
			itemCount: len(batch.Items),
			frame:     frameBytes,
		}
		p.mu.Lock()
		p.inflight = inflight
		p.mu.Unlock()

		if err := p.sendAndAwaitACK(ctx, inflight); err != nil {
			if p.session != nil {
				_ = p.session.Close()
			}
			return err
		}
	}
}

func (p *edgeReplayPump) sendAndAwaitACK(ctx context.Context, inflight *inflightBatch) error {
	nextSend := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var wake <-chan time.Time
		if p.isHubReady() {
			if !time.Now().Before(nextSend) {
				// Only the loop owns this flag. Reader callbacks enqueue ACKs;
				// no callback can commit until SendReplay has returned.
				inflight.sent = true
				if err := p.sendReplay(ctx, inflight.frame); err != nil {
					return fmt.Errorf("send replay batch: %w", err)
				}
				nextSend = time.Now().Add(replayAckTimeout)
			}
			timer.Reset(time.Until(nextSend))
			wake = timer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.readyCh:
		case <-wake:
		case ev := <-p.eventCh:
			delay := replayMinBackoff
			if err := p.processEvent(ctx, inflight, ev, &delay); err != nil {
				return err
			}
			p.mu.Lock()
			remaining := p.inflight
			p.mu.Unlock()
			if remaining == nil {
				return nil
			}
			if ev.kind == "telemetry.retry" {
				nextSend = time.Now().Add(delay)
			}
			// An already committed ACK leaves the current send deadline intact.
		}
		timer.Stop()
	}
}

func (p *edgeReplayPump) processEvent(ctx context.Context, inflight *inflightBatch, ev replayEvent, backoff *time.Duration) error {
	switch ev.kind {
	case "telemetry.retry":
		if ev.generation != p.generation {
			return fmt.Errorf("retry generation mismatch: expected %d, got %d", p.generation, ev.generation)
		}
		if ev.retry.StreamID != p.streamID || (inflight != nil && ev.retry.StreamID != inflight.streamID) {
			return fmt.Errorf("retry stream mismatch: expected %s, got %s", p.streamID, ev.retry.StreamID)
		}
		if inflight == nil || !inflight.sent {
			return errors.New("unexpected retry without a sent batch")
		}
		if int64(ev.retry.CommittedSeq) < inflight.firstSeq-1 || int64(ev.retry.CommittedSeq) > inflight.lastSeq {
			return errors.New("retry cursor outside sent batch")
		}

		// Bound RetryAfterMS BEFORE multiplying to avoid int64 duration overflow
		retryMS := ev.retry.RetryAfterMS
		retryMS = max(replayMinBackoff.Milliseconds(), min(replayMaxBackoff.Milliseconds(), retryMS))
		if backoff != nil {
			*backoff = time.Duration(retryMS) * time.Millisecond
		}
		return nil

	case "telemetry.ack":
		ack := ev.ack
		if ev.generation != p.generation {
			return fmt.Errorf("ack generation mismatch: expected %d, got %d", p.generation, ev.generation)
		}
		if ack.StreamID != p.streamID || (inflight != nil && ack.StreamID != inflight.streamID) {
			return fmt.Errorf("ack stream mismatch: expected %s, got %s", p.streamID, ack.StreamID)
		}

		// Check if this is an ACK for the current in-flight batch
		if inflight != nil && int64(ack.CommittedSeq) == inflight.lastSeq {
			totalOutcomes := ack.AcceptedCount + ack.DuplicateCount + int64(len(ack.Rejected))
			if !inflight.sent {
				return errors.New("ACK cannot cover unsent data")
			}
			if ack.AcceptedCount < 0 || ack.DuplicateCount < 0 || ack.AcceptedCount > MaxBatchEvents || ack.DuplicateCount > MaxBatchEvents || totalOutcomes != int64(inflight.itemCount) {
				return fmt.Errorf("ack outcomes count mismatch: expected %d, got %d", inflight.itemCount, totalOutcomes)
			}
			seenRejections := make(map[Decimal]struct{}, len(ack.Rejected))
			for _, rej := range ack.Rejected {
				if int64(rej.Seq) < inflight.firstSeq || int64(rej.Seq) > inflight.lastSeq {
					return fmt.Errorf("rejected seq %d outside inflight batch [%d, %d]", rej.Seq, inflight.firstSeq, inflight.lastSeq)
				}
				if _, seen := seenRejections[rej.Seq]; seen {
					return fmt.Errorf("duplicate rejected seq in ack: %d", rej.Seq)
				}
				seenRejections[rej.Seq] = struct{}{}
			}

			fence := domain.EdgeReplayFence{
				HubID:                p.hubID,
				ProbeID:              p.probeID,
				StreamID:             p.streamID,
				ConnectionGeneration: int64(p.generation),
			}
			rejections := make([]domain.ProbeReplayRejection, len(ack.Rejected))
			for i, r := range ack.Rejected {
				rejections[i] = domain.ProbeReplayRejection{
					Seq:  int64(r.Seq),
					Code: r.Code,
				}
			}
			result := domain.ProbeReplayResult{
				StreamID:       ack.StreamID,
				CommittedSeq:   int64(ack.CommittedSeq),
				AcceptedCount:  ack.AcceptedCount,
				DuplicateCount: ack.DuplicateCount,
				Rejected:       rejections,
			}
			if err := p.repo.CommitReplayACK(ctx, fence, result); err != nil {
				return fmt.Errorf("commit replay ack failed: %w", err)
			}

			p.mu.Lock()
			p.lastCompletedBatch = &completedBatchInfo{
				streamID:     ack.StreamID,
				committedSeq: int64(ack.CommittedSeq),
				totalEvents:  totalOutcomes,
				rejections:   rejections,
			}
			p.inflight = nil
			p.mu.Unlock()
			return nil
		}

		// Otherwise, check if this is a valid proven duplicate ACK for the previous request
		if p.isProvenDuplicateACK(ack, ev.generation) {
			// Proven duplicate: never prune again, do NOT resend current batch
			return nil
		}

		if inflight != nil {
			return fmt.Errorf("ack committed_seq mismatch: expected %d, got %d", inflight.lastSeq, ack.CommittedSeq)
		}
		return errors.New("unexpected ack with no inflight batch")
	default:
		return fmt.Errorf("unknown event kind: %s", ev.kind)
	}
}

func buildReplayBatchFrame(streamID string, gen Decimal, batch *domain.EdgeReplayBatch) ([]byte, error) {
	if batch == nil {
		return nil, domain.ErrValidation
	}
	if canonicalUUID("stream_id", streamID) != nil || batch.StreamID != streamID {
		return nil, fmt.Errorf("stream ID mismatch or empty: expected %s, got %s: %w", streamID, batch.StreamID, domain.ErrValidation)
	}
	if gen <= 0 {
		return nil, fmt.Errorf("generation must be positive, got %d: %w", gen, domain.ErrValidation)
	}
	count := len(batch.Items)
	if count < 1 || count > MaxBatchEvents {
		return nil, fmt.Errorf("batch event count must be 1..256, got %d: %w", count, domain.ErrValidation)
	}
	if batch.FirstSeq <= 0 || batch.LastSeq < batch.FirstSeq {
		return nil, fmt.Errorf("invalid sequence bounds [%d, %d]: %w", batch.FirstSeq, batch.LastSeq, domain.ErrValidation)
	}
	if batch.LastSeq-batch.FirstSeq != int64(count-1) {
		return nil, fmt.Errorf("sequence range [%d, %d] does not match event count %d: %w", batch.FirstSeq, batch.LastSeq, count, domain.ErrValidation)
	}
	for i, item := range batch.Items {
		expectedSeq := batch.FirstSeq + int64(i)
		if item.Seq != expectedSeq {
			return nil, fmt.Errorf("event %d sequence gap: expected %d, got %d: %w", i, expectedSeq, item.Seq, domain.ErrValidation)
		}
		if len(item.Payload) == 0 || len(item.Payload) > MaxEventBytes {
			return nil, fmt.Errorf("event %d payload invalid size %d (max %d): %w", item.Seq, len(item.Payload), MaxEventBytes, domain.ErrValidation)
		}
	}

	total := 0
	for _, item := range batch.Items {
		total += len(item.Payload)
	}
	if total > MaxBatchBytes {
		return nil, domain.ErrValidation
	}
	var buf bytes.Buffer
	buf.Grow(total + 1024)
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("replay message identity unavailable")
	}
	msgID := id.String()
	sentAt := time.Now().UTC().Format(time.RFC3339Nano)

	buf.WriteString(`{"protocol_version":1,"type":"telemetry.batch","message_id":"`)
	buf.WriteString(msgID)
	buf.WriteString(`","sent_at":"`)
	buf.WriteString(sentAt)
	buf.WriteString(`","connection_generation":"`)
	buf.WriteString(strconv.FormatInt(int64(gen), 10))
	buf.WriteString(`","payload":{"stream_id":"`)
	buf.WriteString(streamID)
	buf.WriteString(`","first_seq":"`)
	buf.WriteString(strconv.FormatInt(batch.FirstSeq, 10))
	buf.WriteString(`","last_seq":"`)
	buf.WriteString(strconv.FormatInt(batch.LastSeq, 10))
	buf.WriteString(`","events":[`)

	for i, item := range batch.Items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(item.Payload)
	}

	buf.WriteString(`]}}`)

	if buf.Len() > MaxBatchBytes {
		return nil, fmt.Errorf("frame exceeds max batch bytes: %d > %d: %w", buf.Len(), MaxBatchBytes, domain.ErrValidation)
	}
	return buf.Bytes(), nil
}
