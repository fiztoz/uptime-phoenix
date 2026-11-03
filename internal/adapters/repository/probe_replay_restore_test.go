package repository_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// TestProbeReplayRestoredCursor reproduces #192 through the production ingest
// entry point (ProbeReplayService.ProcessBatch over the real ProbeReplayStore).
//
// A probe whose data directory is restored from an older backup restarts its
// stream from an older acknowledged cursor. It replays what the backup still
// held (identical events), then numbers new observations with sequences the hub
// has already committed for other events. Those reused sequences must never
// replace committed evidence, but they must not wedge the stream either:
// observations past the hub's cursor are stored again, and genuine replays of
// the same batch are still not stored twice.
func TestProbeReplayRestoredCursor(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			svc, err := services.NewProbeReplayService(r.store, &services.AccessService{})
			if err != nil {
				t.Fatal(err)
			}
			process := func(b domain.ProbeReplayBatch) *domain.ProbeReplayResult {
				t.Helper()
				result, err := svc.ProcessBatch(t.Context(), r.session, b)
				if err != nil {
					t.Fatalf("ProcessBatch %d..%d: %v", b.FirstSeq, b.LastSeq, err)
				}
				return result
			}

			// Before the restore the hub commits seq 1..5.
			if got := process(r.batch(r.observation(1), r.observation(2), r.observation(3), r.observation(4), r.observation(5))); got.CommittedSeq != 5 || got.AcceptedCount != 5 {
				t.Fatalf("pre-restore ingest: %+v", got)
			}
			committedDigests := receiptDigests(t, r)

			// After the restore the probe's cursor is 2. Seq 3 is still the
			// event the backup held; 4 and 5 are new observations under
			// sequences the hub already committed; 6 and 7 are new and past the
			// hub's cursor.
			restored := r.batch(r.observation(3), r.observation(4), r.observation(5), r.observation(6), r.observation(7))
			restored.Events[1].Digest = restoredDigest(4)
			restored.Events[2].Digest = restoredDigest(5)
			result := process(restored)
			if result.CommittedSeq != 7 || result.AcceptedCount != 2 || result.DuplicateCount != 1 || len(result.Rejected) != 2 {
				t.Fatalf("restored ingest: %+v", result)
			}
			for i, want := range []int64{4, 5} {
				if result.Rejected[i].Seq != want || result.Rejected[i].Code != "sequence_reused" {
					t.Fatalf("rejections: %+v", result.Rejected)
				}
			}
			assertRestoredStream(t, r, committedDigests, 7)

			// A lost ACK replays the identical batch: nothing is stored twice
			// and the reused sequences keep their rejection.
			again := process(restored)
			if again.CommittedSeq != 7 || again.AcceptedCount != 0 || again.DuplicateCount != 3 || len(again.Rejected) != 2 {
				t.Fatalf("replayed restored batch: %+v", again)
			}
			assertRestoredStream(t, r, committedDigests, 7)

			// The stream keeps flowing.
			if next := process(r.batch(r.observation(8))); next.CommittedSeq != 8 || next.AcceptedCount != 1 {
				t.Fatalf("post-restore ingest: %+v", next)
			}
			assertRestoredStream(t, r, committedDigests, 8)
		})
	}
}

// TestProbeReplayUnreconcilableStreamLogged keeps the refusal of a batch that
// skips past the hub cursor (nothing may invent the missing sequences) but
// requires it to be logged with the hub cursor, on both engines.
func TestProbeReplayUnreconcilableStreamLogged(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			r.ingest(t, r.batch(r.observation(1), r.observation(2)))
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			_, err := r.store.IngestReplayBatch(t.Context(), r.session, r.batch(r.observation(5)), &services.AccessService{})
			if !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("a batch past the cursor must be refused: %v", err)
			}
			var found map[string]any
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var record map[string]any
				if json.Unmarshal([]byte(line), &record) == nil && record["probe_id"] == r.session.ProbeID && record["level"] == "ERROR" {
					found = record
				}
			}
			if found == nil || found["hub_committed_seq"] != float64(2) || found["first_seq"] != float64(5) || found["stream_id"] != r.session.StreamID {
				t.Fatalf("unreconcilable-stream record %v in %q", found, buf.String())
			}
			if n := replayCount(t, r.f, "probe_observations"); n != 2 {
				t.Fatalf("refused batch stored evidence: %d rows", n)
			}
		})
	}
}

func restoredDigest(seq int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("restored-timeline-event-%d", seq)))
	return hex.EncodeToString(sum[:])
}

func receiptDigests(t *testing.T, r replayFixture) map[int64]string {
	t.Helper()
	var rows []struct {
		Seq    int64  `bun:"seq"`
		Digest string `bun:"digest"`
	}
	if err := r.f.db.NewSelect().Table("probe_telemetry_receipts").Column("seq", "digest").
		Where("probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID).
		Order("seq ASC").Scan(t.Context(), &rows); err != nil {
		t.Fatal(err)
	}
	out := make(map[int64]string, len(rows))
	for _, row := range rows {
		out[row.Seq] = row.Digest
	}
	return out
}

// assertRestoredStream checks persisted state, not the ACK: one observation
// row per committed sequence, the hub cursor, and committed receipts unchanged.
func assertRestoredStream(t *testing.T, r replayFixture, committed map[int64]string, through int64) {
	t.Helper()
	var seqs []int64
	if err := r.f.db.NewSelect().Table("probe_observations").Column("seq").
		Where("probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID).
		Order("seq ASC").Scan(t.Context(), &seqs); err != nil {
		t.Fatal(err)
	}
	if int64(len(seqs)) != through {
		t.Fatalf("observations %v, want one per seq 1..%d", seqs, through)
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("observations %v, want one per seq 1..%d", seqs, through)
		}
	}
	cursor, err := r.store.GetCursor(t.Context(), r.session.ProbeID, r.session.StreamID)
	if err != nil || cursor != through {
		t.Fatalf("hub cursor %d (%v), want %d", cursor, err, through)
	}
	digests := receiptDigests(t, r)
	if int64(len(digests)) != through {
		t.Fatalf("receipts %d, want %d", len(digests), through)
	}
	for seq, digest := range committed {
		if digests[seq] != digest {
			t.Fatalf("receipt %d changed identity", seq)
		}
	}
}
