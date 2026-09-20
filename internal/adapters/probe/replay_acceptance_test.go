package probe

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// This acceptance fixture deliberately uses a separate SQLite connection for
// fault injection. The assertions inspect durable rows, not fake-port calls.
func acceptanceReplayStore(t *testing.T) (*edge.Store, *sql.DB, domain.EdgeReplayFence, [][]byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	fence := domain.EdgeReplayFence{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222", StreamID: "33333333-3333-4333-8333-333333333333", ConnectionGeneration: 7}
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: fence.ProbeID, StreamID: fence.StreamID, Fingerprint: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edge.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_identity SET hub_id=?, connection_generation=?, last_created_seq=3 WHERE id=1", fence.HubID, fence.ConnectionGeneration); err != nil {
		t.Fatal(err)
	}
	var payloads [][]byte
	for seq := int64(1); seq <= 3; seq++ {
		event := TelemetryEvent{Seq: Decimal(seq), Kind: "observation", ObservedAt: Timestamp(time.Now().UTC()), Data: Observation{MonitorID: 1, AssignmentGeneration: 1, ConfigRevision: 1, Status: "UP", RawStatus: "UP", Conditions: []ConditionObservation{}}}
		payload, err := json.MarshalIndent(event, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(t.Context(), "INSERT INTO edge_telemetry_outbox(seq,kind,observed_at,payload) VALUES(?,?,?,?)", seq, event.Kind, time.Time(event.ObservedAt).UnixMicro(), payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	return store, db, fence, payloads
}

func assertAcceptanceReplayProgress(t *testing.T, db *sql.DB, cursor, rows int64) {
	t.Helper()
	var gotCursor, gotRows int64
	if err := db.QueryRowContext(t.Context(), "SELECT committed_seq FROM edge_identity WHERE id=1").Scan(&gotCursor); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM edge_telemetry_outbox").Scan(&gotRows); err != nil {
		t.Fatal(err)
	}
	if gotCursor != cursor || gotRows != rows {
		t.Fatalf("durable cursor/retained rows = %d/%d, want %d/%d", gotCursor, gotRows, cursor, rows)
	}
}

func TestReplayAcceptanceEdgeACKIsAtomicAndFenced(t *testing.T) {
	store, db, fence, payloads := acceptanceReplayStore(t)
	batch, err := store.ReadReplayBatch(t.Context(), 1, 3, 512<<10)
	if err != nil || batch == nil || len(batch.Items) != 3 {
		t.Fatalf("read retained prefix: %+v %v", batch, err)
	}
	for i, item := range batch.Items {
		if !bytes.Equal(item.Payload, payloads[i]) {
			t.Fatal("replay rebuilt exact stored bytes")
		}
	}
	ack := domain.ProbeReplayResult{StreamID: fence.StreamID, CommittedSeq: 3, AcceptedCount: 3}
	wrong := fence
	wrong.ConnectionGeneration--
	if err := store.CommitReplayACK(t.Context(), wrong, ack); err == nil {
		t.Fatal("stale connection acknowledged source history")
	}
	assertAcceptanceReplayProgress(t, db, 0, 3)
	wrong = fence
	wrong.StreamID = fence.HubID
	if err := store.CommitReplayACK(t.Context(), wrong, ack); err == nil {
		t.Fatal("foreign stream acknowledged source history")
	}
	assertAcceptanceReplayProgress(t, db, 0, 3)
	if _, err := db.ExecContext(t.Context(), "CREATE TRIGGER fail_replay_cursor BEFORE UPDATE OF committed_seq ON edge_identity BEGIN SELECT RAISE(ABORT, 'injected cursor commit failure'); END"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = db.ExecContext(ctx, "DROP TRIGGER IF EXISTS fail_replay_cursor")
	})
	if err := store.CommitReplayACK(t.Context(), fence, ack); err == nil {
		t.Fatal("failed cursor write returned success")
	}
	assertAcceptanceReplayProgress(t, db, 0, 3)
	if _, err := db.ExecContext(t.Context(), "DROP TRIGGER fail_replay_cursor"); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitReplayACK(t.Context(), fence, ack); err != nil {
		t.Fatal(err)
	}
	assertAcceptanceReplayProgress(t, db, 3, 0)
}

func TestReplayAcceptanceNeverSkipsMissingSourceEvidence(t *testing.T) {
	store, db, _, _ := acceptanceReplayStore(t)
	if _, err := db.ExecContext(t.Context(), "DELETE FROM edge_telemetry_outbox WHERE seq=2"); err != nil {
		t.Fatal(err)
	}
	batch, err := store.ReadReplayBatch(t.Context(), 1, 3, 512<<10)
	if err == nil && batch != nil && batch.LastSeq > 1 {
		t.Fatal("missing source evidence was skipped")
	}
	assertAcceptanceReplayProgress(t, db, 0, 2)
}

func TestReplayAcceptanceDuplicateACKDoesNotResendCurrent(t *testing.T) {
	repo := &fakeReplayRepo{}
	p := newEdgeReplayPump(repo, nil, "hub", "probe", "stream", 1)
	p.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true})
	p.lastCompletedBatch = &completedBatchInfo{streamID: "stream", committedSeq: 1, totalEvents: 1}
	batch := &inflightBatch{streamID: "stream", firstSeq: 2, lastSeq: 2, itemCount: 1, frame: []byte("exact")}
	p.inflight = batch
	sent := make(chan struct{}, 10)
	p.sendReplay = func(context.Context, []byte) error { sent <- struct{}{}; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.sendAndAwaitACK(ctx, batch) }()
	<-sent
	p.eventCh <- replayEvent{kind: "telemetry.ack", generation: 1, ack: TelemetryACK{StreamID: "stream", CommittedSeq: 1, DuplicateCount: 1}}
	select {
	case <-sent:
		t.Error("duplicate prior ACK immediately resent current batch")
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	<-done
}

func TestReplayAcceptanceACKRemainsResponsiveDuringRetryDelay(t *testing.T) {
	repo := &fakeReplayRepo{}
	p := newEdgeReplayPump(repo, nil, "hub", "probe", "stream", 1)
	p.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true})
	batch := &inflightBatch{streamID: "stream", firstSeq: 1, lastSeq: 1, itemCount: 1, frame: []byte("exact")}
	p.inflight = batch
	sent := make(chan struct{}, 10)
	p.sendReplay = func(context.Context, []byte) error { sent <- struct{}{}; return nil }
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.sendAndAwaitACK(ctx, batch) }()
	<-sent
	p.eventCh <- replayEvent{kind: "telemetry.retry", generation: 1, retry: TelemetryRetry{StreamID: "stream", CommittedSeq: 0, RetryAfterMS: 30000}}
	p.eventCh <- replayEvent{kind: "telemetry.ack", generation: 1, ack: TelemetryACK{StreamID: "stream", CommittedSeq: 1, AcceptedCount: 1}}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		<-done
		t.Fatal("durable ACK blocked behind retry sleep")
	}
}
