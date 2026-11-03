package services_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// captureDefaultLog redirects slog.Default to a JSON buffer for one test.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func loggedRecord(t *testing.T, buf *bytes.Buffer, probeID, level string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if record["probe_id"] == probeID && record["level"] == level {
			return record
		}
	}
	t.Fatalf("no %s record for probe %s in %q", level, probeID, buf.String())
	return nil
}

// #192: a probe restored from an older backup reuses sequence numbers the hub
// already committed. The hub's recovery drops those events, so it must say so
// loudly instead of failing silently.
func TestProbeReplayServiceLogsReusedSequences(t *testing.T) {
	buf := captureDefaultLog(t)
	session := domain.ProbeReplaySession{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222192", StreamID: "33333333-3333-4333-8333-333333333333", OwnerID: "44444444-4444-4444-8444-444444444444", ConnectionGeneration: 1}
	now := time.Now().UTC()
	batch := domain.ProbeReplayBatch{ProbeID: session.ProbeID, StreamID: session.StreamID, FirstSeq: 3, LastSeq: 5, Events: []domain.ProbeReplayEvent{
		{Seq: 3, Kind: domain.ReplayKindObservation, ObservedAt: now, Digest: strings.Repeat("a", 64)},
		{Seq: 4, Kind: domain.ReplayKindObservation, ObservedAt: now, Digest: strings.Repeat("b", 64)},
		{Seq: 5, Kind: domain.ReplayKindObservation, ObservedAt: now, Digest: strings.Repeat("c", 64)},
	}}
	repo := &mockReplayRepo{ingestFn: func(context.Context, domain.ProbeReplaySession, domain.ProbeReplayBatch, ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
		return &domain.ProbeReplayResult{StreamID: batch.StreamID, CommittedSeq: 5, DuplicateCount: 1, Rejected: []domain.ProbeReplayRejection{{Seq: 4, Code: "sequence_reused"}, {Seq: 5, Code: "sequence_reused"}}}, nil
	}}
	svc, err := services.NewProbeReplayService(repo, &services.AccessService{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProcessBatch(t.Context(), session, batch); err != nil {
		t.Fatal(err)
	}
	record := loggedRecord(t, buf, session.ProbeID, "ERROR")
	if record["stream_id"] != session.StreamID || record["reused_count"] != float64(2) || record["first_reused_seq"] != float64(4) || record["last_reused_seq"] != float64(5) {
		t.Fatalf("reused-sequence record: %v", record)
	}
}

// A refused batch closes the session; that must no longer be silent. The log
// reads nothing more: a conflict never earns a cursor read or a retry.
func TestProbeReplayServiceLogsRefusedBatch(t *testing.T) {
	buf := captureDefaultLog(t)
	session := domain.ProbeReplaySession{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222193", StreamID: "33333333-3333-4333-8333-333333333333", OwnerID: "44444444-4444-4444-8444-444444444444", ConnectionGeneration: 1}
	batch := domain.ProbeReplayBatch{ProbeID: session.ProbeID, StreamID: session.StreamID, FirstSeq: 9, LastSeq: 9, Events: []domain.ProbeReplayEvent{{Seq: 9, Kind: domain.ReplayKindObservation, ObservedAt: time.Now().UTC(), Digest: strings.Repeat("a", 64)}}}
	repo := &mockReplayRepo{
		ingestFn: func(context.Context, domain.ProbeReplaySession, domain.ProbeReplayBatch, ports.ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error) {
			return nil, ports.ErrConflict
		},
		cursorFn: func(context.Context, string, string) (int64, error) {
			t.Fatal("a refused batch read the cursor")
			return 0, nil
		},
	}
	svc, err := services.NewProbeReplayService(repo, &services.AccessService{})
	if err != nil {
		t.Fatal(err)
	}
	if result, err := svc.ProcessBatch(t.Context(), session, batch); err == nil || result != nil {
		t.Fatalf("a refused batch must stay refused: %+v %v", result, err)
	}
	record := loggedRecord(t, buf, session.ProbeID, "WARN")
	if record["first_seq"] != float64(9) || record["last_seq"] != float64(9) {
		t.Fatalf("refused-batch record: %v", record)
	}
}
