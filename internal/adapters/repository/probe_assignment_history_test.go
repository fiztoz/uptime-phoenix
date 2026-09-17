package repository_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func assignmentHistory(t *testing.T, f probeRegistryFixture, monitorID int64) []domain.AssignmentInterval {
	t.Helper()
	rows, err := f.assignments.ListHistory(context.Background(), monitorID,
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestAssignmentHistoryPreservesMembershipPolicyAndGeneration(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			start := assignmentHistory(t, f, monitorID)[0].From
			local := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusUp, 0, start)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: local, State: stateFrom(local)}); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "asia")
			replace := func(revision int64, ids []string, policy domain.HealthPolicy) time.Time {
				t.Helper()
				if _, err := f.assignments.Replace(ctx, monitorID, revision, ids, policy); err != nil {
					t.Fatal(err)
				}
				rows := assignmentHistory(t, f, monitorID)
				return rows[len(rows)-1].From
			}
			added := replace(1, []string{"local", remote.ID}, domain.HealthPolicyAnyDown)
			down := regionalSample(monitorID, remote.ID, remoteStreamID, 1, 1, domain.StatusDown, 1, added)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: down, State: stateFrom(down)}); err != nil {
				t.Fatal(err)
			}
			policy := replace(2, []string{remote.ID, "local"}, domain.HealthPolicyAllDown)
			removed := replace(3, []string{"local"}, domain.HealthPolicyAnyDown)
			readded := replace(4, []string{"local", remote.ID}, domain.HealthPolicyAnyDown)
			before := assignmentHistory(t, f, monitorID)
			if len(before) != 8 {
				t.Fatalf("revision snapshots: %+v", before)
			}
			replace(5, []string{remote.ID, "local"}, domain.HealthPolicyAnyDown)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.assignments.Replace(ctx, monitorID, 4, []string{"local"}, domain.HealthPolicyAnyDown); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("stale revision accepted: %v", err)
			}
			if got := assignmentHistory(t, f, monitorID); !reflect.DeepEqual(got, before) {
				t.Fatalf("no-op/failed replacement rewrote history: %+v", got)
			}
			for i, row := range before {
				if row.From.Location() != time.UTC || (!row.To.IsZero() && row.To.Location() != time.UTC) {
					t.Fatalf("non-UTC boundary: %+v", row)
				}
				if i > 0 && row.From.Before(before[i-1].From) {
					t.Fatal("history is not chronologically ordered")
				}
			}
			if before[6].ProbeID != remote.ID || before[6].Generation != 2 || before[7].Generation != 1 {
				t.Fatalf("re-added/retained generations: %+v", before[6:])
			}

			// Query after all edits: old DOWN remains under the original policy;
			// the re-added generation cannot borrow generation one's evidence.
			svc := services.NewMonitorHealthService(newEngineMonitorRepo(f), f.assignments, f.commits, allowAllMonitors{})
			end := readded.Add(time.Millisecond)
			zone := time.FixedZone("UTC+7", 7*3600)
			history, err := svc.History(ctx, 1, monitorID, start.In(zone), end.In(zone))
			if err != nil {
				t.Fatal(err)
			}
			wantStarts := []time.Time{start, added, policy, removed, readded}
			wantStatus := []domain.Status{domain.StatusUp, domain.StatusDown, domain.StatusUp, domain.StatusUp, domain.StatusUnknown}
			wantCauses := []string{domain.HealthHistoryCauseRegional, domain.HealthHistoryCauseAssignment,
				domain.HealthHistoryCausePolicy, domain.HealthHistoryCauseAssignment, domain.HealthHistoryCauseAssignment}
			if len(history.Intervals) != len(wantStarts) {
				t.Fatalf("historical intervals: %+v", history)
			}
			for i, interval := range history.Intervals {
				if !interval.From.Equal(wantStarts[i]) || interval.Status != wantStatus[i] || interval.Cause != wantCauses[i] {
					t.Errorf("interval %d: %+v", i, interval)
				}
				if i+1 < len(wantStarts) && !interval.To.Equal(wantStarts[i+1]) {
					t.Errorf("gap or overlap at %d: %+v", i, interval)
				}
			}
			if history.Durations.Down != policy.Sub(added) || history.Durations.Unknown != end.Sub(readded) ||
				history.Durations.Up+history.Durations.Down+history.Durations.Unknown != end.Sub(start) {
				t.Fatalf("wrong duration/coverage: %+v", history.Durations)
			}
			up := regionalSample(monitorID, remote.ID, remoteStreamID, 2, 2, domain.StatusUp, 0, end)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: up, State: stateFrom(up)}); err != nil {
				t.Fatal(err)
			}
			current, err := svc.Current(ctx, 1, monitorID, end)
			if err != nil || current.Health.Status != domain.StatusUp {
				t.Fatalf("new generation evidence did not restore health: %+v %v", current, err)
			}
			replayed, err := svc.History(ctx, 1, monitorID, start, end)
			if err != nil || !reflect.DeepEqual(history, replayed) {
				t.Fatalf("new generation rewrote historical evidence: %+v %v", replayed, err)
			}
			// Half-open boundaries omit the revision ending at the lower bound,
			// retain original boundaries and don't include the next revision.
			rows, err := f.assignments.ListHistory(ctx, monitorID, policy.In(zone), removed.In(zone))
			if err != nil || len(rows) != 2 || rows[0].Revision != 3 || !rows[0].From.Equal(policy) {
				t.Fatalf("windowed revision: %+v %v", rows, err)
			}
			if err := runNamedMigration(t, f, "040_probe_assignment_history", "down"); err == nil {
				t.Fatal("downgrade discarded assignment history")
			}
			if got := assignmentHistory(t, f, monitorID); !reflect.DeepEqual(got, before) {
				t.Fatal("failed downgrade changed history")
			}
			if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", monitorID); err != nil {
				t.Fatal(err)
			}
			if got := assignmentHistory(t, f, monitorID); len(got) != 0 {
				t.Fatal("deleted monitor retained history")
			}
		})
	}
}

func TestAssignmentHistoryFailureRollsBackEntireReplacement(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			initial, err := f.assignments.InitializeLocal(ctx, monitorID)
			if err != nil {
				t.Fatal(err)
			}
			before := assignmentHistory(t, f, monitorID)
			if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_dirty_buckets"); err != nil {
				t.Fatal(err)
			}
			trigger := "CREATE TRIGGER fail_assignment_history BEFORE INSERT ON monitor_probe_assignment_history BEGIN SELECT RAISE(ABORT, 'injected history failure'); END"
			if engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_assignment_history BEFORE INSERT ON monitor_probe_assignment_history FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected history failure'"
			}
			if _, err := f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_assignment_history"); err != nil {
					t.Error(err)
				}
			}()
			if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local"}, domain.HealthPolicyAllDown); err == nil {
				t.Fatal("history failure reported successful replacement")
			}
			after, err := f.assignments.GetByMonitorID(ctx, monitorID)
			if err != nil || !reflect.DeepEqual(initial, after) || !reflect.DeepEqual(before, assignmentHistory(t, f, monitorID)) {
				t.Fatalf("partial transaction leaked: %+v %v", after, err)
			}
			dirty, err := f.projections.ListDirty(ctx, domain.DirtyResolutionOverall, 10)
			if err != nil || len(dirty) != 0 {
				t.Fatalf("failed write left dirty work: %+v %v", dirty, err)
			}
			monitor := &domain.Monitor{
				UserID: f.user(t), Name: "history-write-must-succeed", Type: "http", Active: true,
				Interval: 60, Timeout: 5, Config: map[string]any{},
			}
			if err := newEngineMonitorRepo(f).Create(ctx, monitor); err == nil {
				t.Fatal("monitor creation ignored history failure")
			}
			exists, err := f.db.NewSelect().Table("monitors").Where("name = ?", monitor.Name).Exists(ctx)
			if err != nil || exists {
				t.Fatalf("failed history left an unassigned monitor: %v %v", exists, err)
			}
		})
	}
}

func TestAssignmentHistoryLocalDowngradeAndConstraints(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			before := assignmentHistory(t, f, monitorID)
			for _, mutation := range []string{
				"generation = 0", "revision = 0", "health_policy = 'invalid'", "ended_at = started_at",
			} {
				if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_history SET "+mutation+" WHERE monitor_id = ?", monitorID); err == nil {
					t.Fatalf("accepted invalid history: %s", mutation)
				}
			}
			for _, window := range [][2]time.Time{{}, {before[0].From, before[0].From}} {
				if _, err := f.assignments.ListHistory(ctx, monitorID, window[0], window[1]); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid window accepted: %v", err)
				}
			}
			if err := runNamedMigration(t, f, "040_probe_assignment_history", "down"); err != nil {
				t.Fatalf("untouched local downgrade: %v", err)
			}
			if err := runNamedMigration(t, f, "040_probe_assignment_history", "up"); err != nil {
				t.Fatal(err)
			}
			if got := assignmentHistory(t, f, monitorID); !reflect.DeepEqual(before, got) {
				t.Fatalf("safe cycle changed local membership: %+v", got)
			}
			otherID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, otherID); err != nil {
				t.Fatal(err)
			}
			if got := assignmentHistory(t, f, monitorID); !reflect.DeepEqual(before, got) {
				t.Fatal("history read leaked another monitor")
			}
		})
	}
}

func TestAssignmentHistoryMigrationBackfillsOnlyKnownRevision(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			if err := runNamedMigration(t, f, "040_probe_assignment_history", "down"); err != nil {
				t.Fatal(err)
			}
			monitorID := f.monitor(t)
			if err := runProbeRegistryMigration(t, f.db, engine, "up"); err != nil {
				t.Fatal(err)
			}
			// Simulate a pre-040 policy edit. No row proves older membership.
			at := time.Date(2026, 9, 16, 10, 0, 0, 123456000, time.UTC)
			if _, err := f.db.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = 3, health_policy = 'all_down', updated_at = ? WHERE monitor_id = ?", at, monitorID); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := runNamedMigration(t, f, "040_probe_assignment_history", "up"); err != nil {
					t.Fatal(err)
				}
			}
			rows := assignmentHistory(t, f, monitorID)
			if len(rows) != 1 || rows[0].Revision != 3 || rows[0].Policy != domain.HealthPolicyAllDown || !rows[0].From.Equal(at) {
				t.Fatalf("backfill invented old revisions: %+v", rows)
			}
			svc := services.NewMonitorHealthService(newEngineMonitorRepo(f), f.assignments, f.commits, allowAllMonitors{})
			history, err := svc.History(ctx, 1, monitorID, at.Add(-time.Minute), at)
			if err != nil || history.Durations.Unknown != time.Minute || len(history.Intervals) != 1 || history.Intervals[0].Reason != "missing_assignment_history" {
				t.Fatalf("unknown pre-migration membership: %+v %v", history, err)
			}
		})
	}
}
