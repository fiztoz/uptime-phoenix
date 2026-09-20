package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func gapBatch(r replayFixture, from, through int64) domain.ProbeReplayBatch {
	g := &domain.ProbeTelemetryGap{StreamID: r.session.StreamID, FromSeq: from, ThroughSeq: through, Reason: "retention_bytes", ObservedFrom: r.at.Add(-time.Hour), ObservedThrough: r.at, AffectedMonitorIDs: []int64{r.monitor}}
	return domain.ProbeReplayBatch{ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, FirstSeq: from, LastSeq: through, Gap: g}
}

func TestProbeReplayGapAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("DurableGapAndOverlap", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				r.ingest(t, r.batch(r.observation(1)))
				b := gapBatch(r, 1, 1000)
				got := r.ingest(t, b)
				if got.CommittedSeq != 1000 || got.AcceptedCount != 0 || got.DuplicateCount != 0 || len(got.Rejected) != 0 {
					t.Fatalf("gap fabricated outcomes: %+v", got)
				}
				for n := 0; n < 2; n++ {
					r.ingest(t, b)
				}
				if replayCount(t, r.f, "probe_observations") != 1 || replayCount(t, r.f, "probe_telemetry_gaps") != 1 || replayCount(t, r.f, "probe_delivery_intents") != 0 {
					t.Fatal("gap erased or duplicated history or generated provider work")
				}
				var row struct {
					FromSeq          int64
					ThroughSeq       int64
					RecomputePending bool
				}
				if err := r.f.db.NewRaw("SELECT from_seq, through_seq, recompute_pending FROM probe_telemetry_gaps").Scan(t.Context(), &row); err != nil {
					t.Fatal(err)
				}
				if row.FromSeq != 2 || row.ThroughSeq != 1000 || !row.RecomputePending {
					t.Fatalf("gap overwrote committed evidence or lost coverage work: %+v", row)
				}
				late := r.ingest(t, r.batch(r.observation(999), r.observation(1000), r.observation(1001)))
				if late.AcceptedCount != 1 || len(late.Rejected) != 2 || late.Rejected[0].Code != "history_gap" {
					t.Fatalf("declared loss resurrected: %+v", late)
				}
				if err := runEngineMigration(t, r.f.db, engine, "055_probe_telemetry_gaps", "down"); err == nil {
					t.Fatal("downgrade discarded gap receipts")
				}
			})
			t.Run("AuthorityAndLateRollback", func(t *testing.T) {
				r := newReplayFixture(t, engine)
				b := gapBatch(r, 1, 3)
				bad := r.session
				bad.ConnectionGeneration++
				if _, err := r.store.IngestReplayBatch(t.Context(), bad, b, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("stale gap accepted", err)
				}
				b.Gap.AffectedMonitorIDs = []int64{r.monitor + 1000}
				if _, err := r.store.IngestReplayBatch(t.Context(), r.session, b, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatal("foreign coverage accepted", err)
				}
				b.Gap.AffectedMonitorIDs = []int64{}
				trigger := "CREATE TRIGGER fail_gap_cursor BEFORE UPDATE ON probe_streams BEGIN SELECT RAISE(ABORT, 'private failure'); END"
				if engine == "mariadb" {
					trigger = "CREATE TRIGGER fail_gap_cursor BEFORE UPDATE ON probe_streams FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'private failure'"
				}
				if _, err := r.f.db.ExecContext(t.Context(), trigger); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_gap_cursor") })
				svc, err := services.NewProbeReplayService(r.store, &services.AccessService{})
				if err != nil {
					t.Fatal(err)
				}
				result, err := svc.ProcessBatch(t.Context(), r.session, b)
				if !errors.Is(err, domain.ErrReplayRetry) || result == nil || result.CommittedSeq != 0 || replayCount(t, r.f, "probe_telemetry_gaps") != 0 {
					t.Fatalf("failed commit acknowledged loss: %+v %v", result, err)
				}
				if _, err := r.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_gap_cursor"); err != nil {
					t.Fatal(err)
				}
				r.ingest(t, b)
				if cursor, err := r.store.GetCursor(t.Context(), r.session.ProbeID, r.session.StreamID); err != nil || cursor != 3 {
					t.Fatal("retry failed", cursor, err)
				}
			})
		})
	}
}
