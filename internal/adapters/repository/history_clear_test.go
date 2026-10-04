package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func clearedObservationCount(t *testing.T, r replayFixture) int {
	t.Helper()
	return replayCount(t, r.f, "probe_observations")
}

func receiptRejectionCode(t *testing.T, r replayFixture, seq int64) string {
	t.Helper()
	var code string
	if err := r.f.db.NewSelect().Table("probe_telemetry_receipts").
		Column("rejection_code").
		Where("probe_id = ? AND stream_id = ? AND seq = ?", r.session.ProbeID, r.session.StreamID, seq).
		Scan(t.Context(), &code); err != nil {
		t.Fatalf("receipt for seq %d: %v", seq, err)
	}
	return code
}

// TestHistoryClear_WatermarkFencesReplay proves the clear-history watermark is
// the fence T36 requires: evidence created before the clear but replayed after
// it is dropped and acknowledged (never resurrected), while genuine post-clear
// evidence keeps flowing. The drop is counted on the fence, and a repeated
// replay of the dropped prefix is discarded idempotently from its receipt.
func TestHistoryClear_WatermarkFencesReplay(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			clears := repository.NewHistoryClearStore(r.f.db)

			// Two observations are ingested before the clear.
			if res := r.ingest(t, r.batch(r.observation(1), r.observation(2))); res.AcceptedCount != 2 {
				t.Fatalf("pre-clear ingest: %+v", res)
			}
			clearAt := r.at.Add(2 * time.Second)
			fences, err := clears.ClearMonitorHistory(ctx, r.monitor, clearAt)
			if err != nil {
				t.Fatalf("clear: %v", err)
			}
			if len(fences) != 1 || fences[0].ProbeID != r.session.ProbeID || fences[0].AssignmentGeneration != 1 {
				t.Fatalf("fences: %+v", fences)
			}
			if fences[0].ThroughSeq != 2 || !fences[0].ThroughObservedAt.Equal(clearAt.Truncate(time.Microsecond)) {
				t.Fatalf("fence bounds: %+v", fences[0])
			}
			if n := clearedObservationCount(t, r); n != 0 {
				t.Fatalf("cleared evidence still present: %d rows", n)
			}

			// seq 3 was created before the clear (observed_at < clear time) but
			// arrives afterwards — the classic replayed/backlogged queue. The
			// sequence fence alone would miss it (3 > 2); the observation bound
			// must catch it.
			queued := r.observation(3)
			queued.Observation.ObservedAt = r.at.Add(time.Second)
			queued.ObservedAt = queued.Observation.ObservedAt
			res := r.ingest(t, r.batch(queued))
			if res.CommittedSeq != 3 || res.AcceptedCount != 0 || len(res.Rejected) != 1 || res.Rejected[0].Code != "history_cleared" {
				t.Fatalf("queued pre-clear evidence: %+v", res)
			}
			if n := clearedObservationCount(t, r); n != 0 {
				t.Fatalf("cleared evidence resurrected: %d rows", n)
			}
			if code := receiptRejectionCode(t, r, 3); code != "history_cleared" {
				t.Fatalf("drop not acknowledged by receipt: %q", code)
			}
			var drops int64
			if err := r.f.db.NewSelect().Table("history_clear_watermarks").
				Column("dropped_count").Scan(ctx, &drops); err != nil {
				t.Fatal(err)
			}
			if drops != 1 {
				t.Fatalf("intentional drops not counted: %d", drops)
			}

			// Genuine post-clear evidence (observed after the fence) flows.
			fresh := r.observation(4)
			fresh.Observation.ObservedAt = clearAt.Add(time.Second)
			fresh.ObservedAt = fresh.Observation.ObservedAt
			res = r.ingest(t, r.batch(fresh))
			if res.AcceptedCount != 1 || len(res.Rejected) != 0 {
				t.Fatalf("post-clear evidence refused: %+v", res)
			}
			if n := clearedObservationCount(t, r); n != 1 {
				t.Fatalf("post-clear evidence missing: %d rows", n)
			}

			// Replaying the dropped prefix is discarded idempotently from its
			// receipt: still acknowledged as intentional, counted only once.
			res = r.ingest(t, r.batch(queued))
			if res.AcceptedCount != 0 || res.DuplicateCount != 0 || len(res.Rejected) != 1 || res.Rejected[0].Code != "history_cleared" {
				t.Fatalf("replayed drop: %+v", res)
			}
			if err := r.f.db.NewSelect().Table("history_clear_watermarks").
				Column("dropped_count").Scan(ctx, &drops); err != nil {
				t.Fatal(err)
			}
			if drops != 1 {
				t.Fatalf("drop double counted: %d", drops)
			}
		})
	}
}

// TestHistoryClear_FenceIsScopedAndWidens proves the fence is per assignment
// generation (a recreated assignment starts a fresh evidence identity and is
// never fenced by the previous generation's clear), that a repeated clear only
// widens the bound, and that a backward hub clock can never reopen it.
func TestHistoryClear_FenceIsScopedAndWidens(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			clears := repository.NewHistoryClearStore(r.f.db)

			// Generation-one evidence, then a clear through it.
			if res := r.ingest(t, r.batch(r.observation(1))); res.AcceptedCount != 1 {
				t.Fatalf("pre-clear ingest: %+v", res)
			}
			clearAt := r.at.Add(2 * time.Second)
			if _, err := clears.ClearMonitorHistory(ctx, r.monitor, clearAt); err != nil {
				t.Fatalf("first clear: %v", err)
			}

			// Recreate the assignment: remove the remote member and re-add it so
			// its generation advances to two — a new evidence identity. Rewind the
			// history interval like the fixture does so evidence stamped before
			// the clear is still inside the generation-two interval and the
			// authorizer accepts it on its own merits.
			set, err := r.f.assignments.GetByMonitorID(ctx, r.monitor)
			if err != nil {
				t.Fatal(err)
			}
			interim, err := r.f.assignments.Replace(ctx, r.monitor, set.Revision, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown)
			if err != nil {
				t.Fatal(err)
			}
			readded, err := r.f.assignments.Replace(ctx, r.monitor, interim.Revision, []string{r.session.ProbeID}, domain.HealthPolicyAnyDown)
			if err != nil {
				t.Fatal(err)
			}
			if readded.Assignments[0].Generation != 2 {
				t.Fatalf("assignment generation not recreated: %+v", readded.Assignments)
			}
			if _, err := r.f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_history SET started_at = ?", r.at.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			// Republish the desired configuration for the recreated assignment
			// and stamp the evidence with the resulting revision, exactly as the
			// source would.
			meta, err := r.syncer.RefreshRemote(ctx, syncTarget(), r.at.Add(-time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			freshGeneration := r.observation(2)
			freshGeneration.Observation.AssignmentGeneration = 2
			freshGeneration.Observation.ConfigRevision = meta.Revision
			freshGeneration.Observation.ObservedAt = r.at
			freshGeneration.ObservedAt = r.at
			res := r.ingest(t, r.batch(freshGeneration))
			if res.AcceptedCount != 1 || len(res.Rejected) != 0 {
				t.Fatalf("new generation fenced by the old clear: rejected=%+v accepted=%d config=%d", res.Rejected, res.AcceptedCount, meta.Revision)
			}

			// A second clear covers the active generation-two assignment and its
			// ingested evidence. Clearing widens; it never installs a smaller
			// bound.
			second := clearAt.Add(time.Second)
			fences, err := clears.ClearMonitorHistory(ctx, r.monitor, second)
			if err != nil {
				t.Fatalf("second clear: %v", err)
			}
			if len(fences) != 1 || fences[0].AssignmentGeneration != 2 || fences[0].ThroughSeq != 2 || !fences[0].ThroughObservedAt.Equal(second.Truncate(time.Microsecond)) {
				t.Fatalf("generation-two fence: %+v", fences)
			}

			// A third clear stamped in the past keeps every bound: a backward hub
			// clock can never reopen the fence.
			fences, err = clears.ClearMonitorHistory(ctx, r.monitor, r.at)
			if err != nil {
				t.Fatalf("backward-clock clear: %v", err)
			}
			if len(fences) != 1 || fences[0].AssignmentGeneration != 2 {
				t.Fatalf("installed fences: %+v", fences)
			}
			if fences[0].ThroughSeq != 2 || !fences[0].ThroughObservedAt.Equal(second.Truncate(time.Microsecond)) {
				t.Fatalf("backward clock shrank the fence: %+v", fences[0])
			}
			// The retired generation keeps its own fence untouched.
			var gen1At time.Time
			if err := r.f.db.NewSelect().Table("history_clear_watermarks").
				Column("through_observed_at").
				Where("monitor_id = ? AND assignment_generation = 1", r.monitor).
				Scan(ctx, &gen1At); err != nil {
				t.Fatal(err)
			}
			if !gen1At.Equal(clearAt.Truncate(time.Microsecond)) {
				t.Fatalf("generation-one fence moved: %v", gen1At)
			}
		})
	}
}

// TestHistoryClear_UnknownMonitorIsNotFound proves the destructive action is
// refused for a monitor that does not exist instead of deleting nothing and
// reporting success.
func TestHistoryClear_UnknownMonitorIsNotFound(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			_, err := repository.NewHistoryClearStore(r.f.db).ClearMonitorHistory(t.Context(), 999999, time.Now().UTC())
			if err == nil {
				t.Fatal("clear of an unknown monitor reported success")
			}
		})
	}
}
