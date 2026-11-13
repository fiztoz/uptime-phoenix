package repository_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// T32: probes that check at different rates must not weigh in by sample count.
// Probe A checks every 15 s and stays UP (40 samples in 10 min); probe B checks
// every 60 s and is DOWN for minutes 3-6 (4 of its 10 samples). Under ANY_DOWN
// the overall monitor is DOWN for exactly those 4 minutes, so overall uptime is
// 5/9 over the first 9 minutes. Pooling the raw samples would instead give
// 4 DOWN out of 50 (92% "up"). Each probe's own minute rollup also counts
// durations, not samples: a full minute is 60 s whether it held 4 checks or 1.
func TestProbeHistoryUnevenProbeSamplesAreDurationWeighted(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			const samplesA = 40 // every 15 s for 10 min
			events := make([]domain.ProbeReplayEvent, 0, samplesA)
			for seq := int64(1); seq <= samplesA; seq++ {
				events = append(events, replayObservationAt(r, seq, (seq-1)*15, domain.StatusUp))
			}
			if result := r.ingest(t, r.batch(events...)); result.AcceptedCount != samplesA {
				t.Fatalf("probe A samples: %+v", result)
			}
			probeB := cloneHistoryProbe(t, r, 2, samplesA)
			// B keeps one sample a minute (seq 1, 5, 9, ... at 0 s, 60 s, ...),
			// DOWN at minutes 3, 4, 5 and 6.
			for _, stmt := range []string{
				fmt.Sprintf("DELETE FROM probe_observations WHERE probe_id = '%s' AND MOD(seq, 4) <> 1", probeB),
				fmt.Sprintf("UPDATE probe_observations SET status = %d, raw_status = %d, down_count = 1 WHERE probe_id = '%s' AND seq IN (13, 17, 21, 25)", domain.StatusDown, domain.StatusDown, probeB),
			} {
				if _, err := r.f.db.ExecContext(t.Context(), stmt); err != nil {
					t.Fatal(stmt, err)
				}
			}
			var countB int
			if err := r.f.db.NewRaw("SELECT COUNT(*) FROM probe_observations WHERE probe_id = ?", probeB).Scan(t.Context(), &countB); err != nil || countB != 10 {
				t.Fatalf("probe B samples = %d, %v", countB, err)
			}

			store := repository.NewRegionalCommitStore(r.f.db)
			service := services.NewProbeHistoryService(store)
			for n := 0; ; n++ {
				if n > 100 {
					t.Fatal("history did not converge")
				}
				processed, err := store.ProcessHistoryWork(t.Context(), time.Now().UTC(), 100, service)
				if err != nil {
					t.Fatal(err)
				}
				if processed == 0 {
					break
				}
			}

			full := time.Minute.Microseconds()
			for minute := 0; minute < 9; minute++ {
				bucket := r.at.Add(time.Duration(minute) * time.Minute)
				a := readHistoryAggregateFor(t, r, r.session.ProbeID, bucket)
				b := readHistoryAggregateFor(t, r, probeB, bucket)
				if a.TotalChecks != 4 || a.UpUS != full || a.DownUS != 0 {
					t.Errorf("probe A minute %d: %d checks, up=%dus down=%dus", minute, a.TotalChecks, a.UpUS, a.DownUS)
				}
				bDown := minute >= 3 && minute <= 6
				if b.TotalChecks != 1 || (bDown && b.DownUS != full) || (!bDown && b.UpUS != full) {
					t.Errorf("probe B minute %d: %d checks, up=%dus down=%dus", minute, b.TotalChecks, b.UpUS, b.DownUS)
				}
			}

			from, to := r.at, r.at.Add(9*time.Minute)
			intervals, err := r.f.projections.ListHealthHistory(t.Context(), r.monitor, from, to)
			if err != nil {
				t.Fatal(err)
			}
			var up, down, other time.Duration
			var downFrom, downTo time.Time
			for _, interval := range intervals {
				start, end := interval.From, interval.To
				if start.Before(from) {
					start = from
				}
				if end.IsZero() || end.After(to) {
					end = to
				}
				if !start.Before(end) {
					continue
				}
				switch interval.Status {
				case domain.StatusUp:
					up += end.Sub(start)
				case domain.StatusDown:
					down += end.Sub(start)
					if downFrom.IsZero() {
						downFrom = start
					}
					downTo = end
				default:
					other += end.Sub(start)
				}
			}
			// The projection stores one row per minute; 4 min of DOWN between
			// minute 3 and minute 7 means one unbroken outage.
			if !downFrom.Equal(r.at.Add(3*time.Minute)) || !downTo.Equal(r.at.Add(7*time.Minute)) {
				t.Errorf("overall DOWN %s..%s, want minute 3..7", downFrom, downTo)
			}
			if down != 4*time.Minute || up != 5*time.Minute || other != 0 {
				t.Fatalf("overall up=%s down=%s other=%s, want 5m/4m/0 (sample pooling would give 46 of 50 up): %+v", up, down, other, intervals)
			}
		})
	}
}

func readHistoryAggregateFor(t *testing.T, r replayFixture, probeID string, at time.Time) repository.AggregateModel {
	t.Helper()
	var row repository.AggregateModel
	if err := r.f.db.NewSelect().Model(&row).ModelTableExpr("heartbeat_1m AS aggregate_model").Where("monitor_id = ? AND probe_id = ? AND bucket = ?", r.monitor, probeID, at.UTC()).Scan(t.Context()); err != nil {
		t.Fatal(probeID, at, err)
	}
	return row
}
