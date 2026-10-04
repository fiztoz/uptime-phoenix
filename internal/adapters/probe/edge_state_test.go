package probe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type currentReadFunc func(context.Context, domain.EdgeReplayFence, time.Time) (domain.EdgeCurrentSnapshot, error)

func (f currentReadFunc) ReadCurrentSnapshot(ctx context.Context, fence domain.EdgeReplayFence, at time.Time) (domain.EdgeCurrentSnapshot, error) {
	return f(ctx, fence, at)
}

func TestEdgeStateReceiptDuplicateCannotAcknowledgeNextTransfer(t *testing.T) {
	pump := newEdgeStatePump(nil, nil, domain.EdgeReplayFence{ConnectionGeneration: 1}, 1, nil)
	receipt := StateApplied{StateTransferIdentity: StateTransferIdentity{SnapshotID: uuid.NewString(), StreamID: uuid.NewString(), ConfigRevision: 1}, SHA256: strings.Repeat("a", 64)}
	pump.pending = &receipt
	if err := pump.handleApplied(1, receipt); err != nil {
		t.Fatal(err)
	}
	<-pump.receipts
	// The sender has received the first ACK but has not yet acquired mu to
	// clear pending. A duplicate arriving in this interval must not remain
	// queued to acknowledge the next snapshot.
	if err := pump.handleApplied(1, receipt); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pump.receipts:
		t.Fatal("duplicate ACK escaped its transfer and can acknowledge the next snapshot")
	default:
	}
}

func TestEdgeStatePumpWaitsForConfirmedConfigAndSingleReceipt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	connections := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err == nil {
			connections <- c
		}
	}))
	defer server.Close()
	client, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.CloseNow() }()
	var conn *websocket.Conn
	select {
	case conn = <-connections:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	session, err := NewSession(conn, SessionConfig{Generation: 1, PeerRole: "hub"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	fence := domain.EdgeReplayFence{HubID: uuid.NewString(), ProbeID: uuid.NewString(), StreamID: uuid.NewString(), ConnectionGeneration: 1}
	var reads atomic.Int32
	repo := currentReadFunc(func(ctx context.Context, f domain.EdgeReplayFence, at time.Time) (domain.EdgeCurrentSnapshot, error) {
		reads.Add(1)
		return domain.EdgeCurrentSnapshot{Identity: domain.EdgeIdentity{HubID: f.HubID, ProbeID: f.ProbeID, StreamID: f.StreamID, ConfigRevision: 1}, CreatedAt: at, States: []domain.EdgeCurrentEvidence{}}, ctx.Err()
	})
	history := &fakeReplayRepo{}
	replay := newEdgeReplayPump(history, nil, fence.HubID, fence.ProbeID, fence.StreamID, 1)
	replay.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, ConfigRevision: 1})
	pump := newEdgeStatePump(repo, session, fence, 1, replay)
	pump.interval = 20 * time.Millisecond
	pump.timeout = time.Second
	sessionDone := make(chan error, 1)
	go func() {
		sessionDone <- session.Run(ctx, func(ctx context.Context, env Envelope) error { return ctx.Err() })
	}()
	defer func() { cancel(); _ = session.Close(); <-sessionDone }()
	pumpDone := make(chan error, 1)
	go func() { pumpDone <- pump.run(ctx) }()
	frames := make(chan []byte, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_, data, err := client.Read(ctx)
			if err != nil {
				return
			}
			select {
			case frames <- data:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); _ = client.CloseNow(); <-readerDone }()
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, ConfigRevision: 0})
	select {
	case <-frames:
		t.Fatal("desired but unconfirmed config authorized state")
	case <-time.After(50 * time.Millisecond):
	}
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, ConfigRevision: 1})
	next := func() []byte {
		t.Helper()
		select {
		case data := <-frames:
			return data
		case err := <-pumpDone:
			t.Fatalf("state pump stopped: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return nil
	}
	_, begin, err := DecodeStateBegin(next())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStateChunk(next()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStateCommit(next()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-frames:
		t.Fatal("a second state transfer overlapped its receipt")
	case <-time.After(80 * time.Millisecond):
	}
	if reads.Load() != 1 || replay.isHubReady() {
		t.Fatal("backlog escaped initial state receipt gate")
	}
	receipt := StateApplied{StateTransferIdentity: begin.StateTransferIdentity, SHA256: begin.SHA256, StateCount: 0, AppliedAt: Timestamp(time.Now().UTC())}
	forged := receipt
	forged.SnapshotID = uuid.NewString()
	if err := pump.handleApplied(1, forged); err == nil {
		t.Fatal("unsolicited state receipt accepted")
	}
	if err := pump.handleApplied(2, receipt); err == nil {
		t.Fatal("stale generation receipt accepted")
	}
	if err := pump.handleApplied(1, receipt); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStateBegin(next()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStateChunk(next()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeStateCommit(next()); err != nil {
		t.Fatal(err)
	}
	if !replay.isHubReady() {
		t.Fatal("initial state receipt did not release replay")
	}
	if err := pump.handleApplied(1, receipt); err != nil {
		t.Fatal("committed duplicate receipt rejected", err)
	}
	select {
	case err := <-pumpDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("missing receipt did not expire", err)
		}
	case <-ctx.Done():
		t.Fatal("state receipt timeout did not fire")
	}
	if reads.Load() != 2 {
		t.Fatal("missing receipt allowed overlapping snapshots", reads.Load())
	}
	history.mu.Lock()
	commits := history.commitACKCalls
	history.mu.Unlock()
	if commits != 0 {
		t.Fatal("state receipt acknowledged history")
	}
}
