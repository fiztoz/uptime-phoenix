package probe

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestHubTransportPeerHealthCannotMaskIngestFailure(t *testing.T) {
	s := setupControlledHubPeer(t)
	s.input.CommittedSeq = 9
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	var attempts atomic.Int32
	ingest := func(_ context.Context, b domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
		if attempts.Add(1) == 1 {
			return &domain.ProbeReplayResult{StreamID: b.StreamID, CommittedSeq: 9}, domain.ErrReplayRetry
		}
		return &domain.ProbeReplayResult{StreamID: b.StreamID, CommittedSeq: b.LastSeq}, nil
	}
	done := make(chan error, 1)
	go func() {
		done <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, ingest)
	}()
	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = conn.CloseNow() }()
	performHandshakeAndConfig(ctx, t, conn, s)
	read := func() (Envelope, []byte) {
		t.Helper()
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		env, err := DecodeEnvelope(data)
		if err != nil {
			t.Fatal(err)
		}
		return env, data
	}
	for {
		env, data := read()
		if env.Type == "health" {
			_, h, err := DecodeHealth(data)
			if err != nil {
				t.Fatal(err)
			}
			if h.Ready && h.ConfigRevision == s.snapshot.Revision {
				break
			}
		}
	}
	gap := TelemetryGap{StreamID: s.input.Connection.StreamID, FromSeq: 10, ThroughSeq: 15, Reason: "retention_bytes", ObservedFrom: Timestamp(time.Now().UTC().Add(-time.Hour)), ObservedThrough: Timestamp(time.Now().UTC()), AffectedMonitorIDs: []int64{101}}
	frame, err := encodeFrame("telemetry.gap", 1, gap)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	degraded, retry := false, false
	for !degraded || !retry {
		env, data := read()
		switch env.Type {
		case "health":
			_, h, err := DecodeHealth(data)
			if err != nil {
				t.Fatal(err)
			}
			if !h.Ready && !h.DBWritable && slices.Contains(h.Errors, "ingest_unavailable") && h.CommittedSeq == 9 {
				degraded = true
			}
		case "telemetry.retry":
			retry = true
		default:
			t.Fatalf("failed commit emitted %s", env.Type)
		}
	}
	// Repeated genuine peer-health frames renew authority while the local write
	// path is still failed. Await an actual hub health frame, not a transport pong.
	healthy := true
	zero := int64(0)
	for range 7 {
		peer := Health{Role: "probe", Ready: true, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: s.snapshot.Revision, CommittedSeq: 9, QueueBytes: &zero, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}
		payload, err := encodeFrame("health", 1, peer)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
			t.Fatal(err)
		}
	}
	env, data := read() // healthy peer traffic must not turn hub ingest health green
	if env.Type != "health" {
		t.Fatalf("expected application health, got %s", env.Type)
	}
	_, h, err := DecodeHealth(data)
	if err != nil || h.Ready || h.DBWritable || h.CommittedSeq != 9 || !slices.Contains(h.Errors, "ingest_unavailable") {
		t.Fatalf("peer masked local ingest failure: %+v %v", h, err)
	}
	// Controlled elapsed inputs demonstrate why the advertised value matters;
	// this is not a claim of a real 90-second outage or a persisted watchdog run.
	timer, err := services.NewProbeWatchdogTimer(domain.DefaultProbeWatchdogTiming(), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := timer.Health(0, true); err != nil {
		t.Fatal(err)
	}
	for elapsed := 15 * time.Second; elapsed <= 90*time.Second; elapsed += 15 * time.Second {
		if err := timer.Health(elapsed, h.Ready && h.DBWritable); err != nil {
			t.Fatal(err)
		}
	}
	eval, err := timer.Evaluate(90*time.Second, false)
	if err != nil || eval.Action != domain.ProbeWatchdogOpen {
		t.Fatalf("degraded ingest cannot trigger watchdog: %+v %v", eval, err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	recovered, acked := false, false
	for !recovered || !acked {
		env, data := read()
		switch env.Type {
		case "health":
			_, h, err := DecodeHealth(data)
			if err != nil {
				t.Fatal(err)
			}
			recovered = h.Ready && h.DBWritable && h.CommittedSeq == 15
		case "telemetry.ack":
			_, ack, err := DecodeTelemetryACK(data)
			if err != nil || ack.CommittedSeq != 15 {
				t.Fatalf("invalid recovery ACK: %+v %v", ack, err)
			}
			acked = true
		default:
			t.Fatalf("unexpected recovery response: %s", env.Type)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("unexpected attempts: %d", attempts.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "connection ended") {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not stop")
	}
}

func TestEdgeReplayExplicitRetryWhileHubDegraded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repo := &fakeReplayRepo{}
	p := newEdgeReplayPump(repo, nil, uuid.NewString(), uuid.NewString(), uuid.NewString(), 1)
	p.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true})
	frame := []byte("exact retained batch")
	batch := &inflightBatch{streamID: p.streamID, firstSeq: 1, lastSeq: 1, itemCount: 1, frame: frame}
	p.inflight = batch
	sent := make(chan []byte, 4)
	p.sendReplay = func(_ context.Context, b []byte) error { sent <- append([]byte(nil), b...); return nil }
	done := make(chan error, 1)
	go func() { done <- p.sendAndAwaitACK(ctx, batch) }()
	receive := func() {
		t.Helper()
		select {
		case b := <-sent:
			if !bytes.Equal(b, frame) {
				t.Fatal("retry rewrote retained bytes")
			}
		case <-ctx.Done():
			t.Fatal("replay stalled", ctx.Err())
		}
	}
	receive()
	p.updateHubHealth(Health{Role: "hub", Ready: false, DBWritable: false, Errors: []string{"ingest_unavailable"}})
	p.setStatePending(true)
	p.eventCh <- replayEvent{kind: "telemetry.retry", generation: 1, retry: TelemetryRetry{StreamID: p.streamID, CommittedSeq: 0, RetryAfterMS: 100}}
	select {
	case <-sent:
		t.Fatal("retry bypassed current-snapshot priority")
	case <-time.After(150 * time.Millisecond):
	}
	p.setStatePending(false)
	receive()
	if p.isHubReady() {
		t.Fatal("retry fabricated healthy hub state")
	}
	repo.mu.Lock()
	count := repo.commitACKCalls
	repo.mu.Unlock()
	if count != 0 {
		t.Fatal("retry pruned unacknowledged evidence")
	}
	p.eventCh <- replayEvent{kind: "telemetry.ack", generation: 1, ack: TelemetryACK{StreamID: p.streamID, CommittedSeq: 1, AcceptedCount: 1}}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.commitACKCalls != 1 || repo.lastResult.CommittedSeq != 1 {
		t.Fatalf("recovery did not commit exactly one ACK: %+v", repo.lastResult)
	}
}
