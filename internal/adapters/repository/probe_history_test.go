package repository_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func oldHistoryFixture(t *testing.T, engine string) replayFixture {
	t.Helper()
	at := time.Now().UTC().Truncate(24 * time.Hour).Add(-3 * 24 * time.Hour)
	r := newReplayFixtureAt(t, engine, at)
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM monitor_probe_assignment_history WHERE ended_at IS NOT NULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets"); err != nil {
		t.Fatal(err)
	}
	return r
}

func replayObservationAt(r replayFixture, seq, seconds int64, status domain.Status) domain.ProbeReplayEvent {
	event := r.observation(seq)
	event.ObservedAt = r.at.Add(time.Duration(seconds) * time.Second)
	event.Observation.ObservedAt = event.ObservedAt
	event.Observation.Status = status
	event.Observation.RawStatus = status
	event.Observation.Ping = 20
	if status == domain.StatusDown {
		event.Observation.DownCount = 1
	}
	return event
}

func drainHistory(t *testing.T, r replayFixture) {
	t.Helper()
	service := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db))
	for n := 0; n < 30; n++ {
		processed, err := service.ProcessBatch(t.Context(), time.Now().UTC(), 100)
		if err != nil {
			t.Fatal(err)
		}
		if processed == 0 {
			return
		}
	}
	t.Fatal("bounded history batches did not converge")
}

func readHistoryAggregate(t *testing.T, r replayFixture, resolution string, at time.Time) repository.AggregateModel {
	t.Helper()
	var row repository.AggregateModel
	if err := r.f.db.NewSelect().Model(&row).ModelTableExpr("heartbeat_"+resolution+" AS aggregate_model").Where("monitor_id = ? AND probe_id = ? AND bucket = ?", r.monitor, r.session.ProbeID, at.UTC()).Scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestProbeHistoryAcceptance(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ReplayAndGapBeyondRollupLookback", func(t *testing.T) {
				r := oldHistoryFixture(t, engine)
				first := r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
				if first.AcceptedCount != 1 {
					t.Fatal("old retained observation rejected", first)
				}
				gap := domain.ProbeTelemetryGap{StreamID: r.session.StreamID, FromSeq: 2, ThroughSeq: 3, Reason: "retention_age", ObservedFrom: r.at.Add(20 * time.Second), ObservedThrough: r.at.Add(30 * time.Second), AffectedMonitorIDs: []int64{r.monitor}}
				r.ingest(t, domain.ProbeReplayBatch{ProbeID: r.session.ProbeID, StreamID: r.session.StreamID, FirstSeq: 2, LastSeq: 3, Gap: &gap})
				result := r.ingest(t, r.batch(replayObservationAt(r, 4, 50, domain.StatusDown), replayObservationAt(r, 5, 70, domain.StatusUp)))
				if result.AcceptedCount != 2 {
					t.Fatal("retained evidence rejected", result)
				}
				drainHistory(t, r)
				minute := readHistoryAggregate(t, r, "1m", r.at)
				if minute.UpUS != 20*time.Second.Microseconds() || minute.UnknownUS != 30*time.Second.Microseconds() || minute.DownUS != 10*time.Second.Microseconds() || minute.TotalChecks != 2 || !minute.HistoryManaged {
					t.Fatalf("minute loss coverage wrong: %+v", minute)
				}
				hour := readHistoryAggregate(t, r, "1h", r.at)
				day := readHistoryAggregate(t, r, "1d", r.at)
				if hour.UpUS <= minute.UpUS || hour.DownUS != 20*time.Second.Microseconds() || hour.UnknownUS == 0 || day.UpUS != hour.UpUS || day.DownUS != hour.DownUS || day.UnknownUS <= hour.UnknownUS {
					t.Fatalf("higher resolutions stale: hour=%+v day=%+v", hour, day)
				}
				intervals, err := r.f.projections.ListHealthHistory(t.Context(), r.monitor, r.at, r.at.Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				var unknown time.Duration
				for _, interval := range intervals {
					if interval.Status == domain.StatusUnknown {
						unknown += interval.To.Sub(interval.From)
					}
				}
				if unknown != 30*time.Second {
					t.Fatalf("overall loss coverage wrong: %s %+v", unknown, intervals)
				}
				var pending int
				if err := r.f.db.NewRaw("SELECT COUNT(*) FROM probe_telemetry_gaps WHERE recompute_pending = ?", true).Scan(t.Context(), &pending); err != nil || pending != 0 {
					t.Fatal("gap cursor did not complete", pending, err)
				}
				cursor, err := r.store.GetCursor(t.Context(), r.session.ProbeID, r.session.StreamID)
				if err != nil || cursor != 5 || replayCount(t, r.f, "probe_delivery_intents") != 0 || replayCount(t, r.f, "probe_observations") != 3 {
					t.Fatal("projection changed source/history/provider state", cursor, err)
				}
				// Idempotent replay cannot dirty the completed history again.
				r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
				if n, err := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db)).ProcessBatch(t.Context(), time.Now().UTC(), 100); err != nil || n != 0 {
					t.Fatal("duplicate replay created work", n, err)
				}
				late := r.ingest(t, r.batch(replayObservationAt(r, 6, 25, domain.StatusDown)))
				if late.AcceptedCount != 1 {
					t.Fatal("late backward-clock observation rejected", late)
				}
				drainHistory(t, r)
				minute = readHistoryAggregate(t, r, "1m", r.at)
				hour = readHistoryAggregate(t, r, "1h", r.at)
				day = readHistoryAggregate(t, r, "1d", r.at)
				if minute.UnknownUS != 5*time.Second.Microseconds() || minute.DownUS != 35*time.Second.Microseconds() || hour.DownUS <= 20*time.Second.Microseconds() || day.DownUS != hour.DownUS {
					t.Fatalf("late replay did not refresh all resolutions: minute=%+v hour=%+v day=%+v", minute, hour, day)
				}
				if err := runEngineMigration(t, r.f.db, engine, "057_probe_history_coverage", "down"); err == nil {
					t.Fatal("downgrade discarded duration coverage")
				}
			})
			t.Run("LastWriteRollbackAndRestart", func(t *testing.T) {
				r := oldHistoryFixture(t, engine)
				r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
				trigger := "CREATE TRIGGER fail_history_consume BEFORE DELETE ON probe_dirty_buckets BEGIN SELECT RAISE(ABORT,'history consume fault'); END"
				if engine == "mariadb" {
					trigger = "CREATE TRIGGER fail_history_consume BEFORE DELETE ON probe_dirty_buckets FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='history consume fault'"
				}
				if _, err := r.f.db.ExecContext(t.Context(), trigger); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _, _ = r.f.db.ExecContext(context.Background(), "DROP TRIGGER IF EXISTS fail_history_consume") })
				if _, err := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db)).ProcessBatch(t.Context(), time.Now().UTC(), 1); err == nil {
					t.Fatal("last write failure returned success")
				}
				if replayCount(t, r.f, "heartbeat_1m") != 0 || replayCount(t, r.f, "heartbeat_1h") != 0 || replayCount(t, r.f, "heartbeat_1d") != 0 || replayCount(t, r.f, "monitor_health_history") != 0 || replayCount(t, r.f, "probe_dirty_buckets") != 4 {
					t.Fatal("partial projection survived rollback")
				}
				if _, err := r.f.db.ExecContext(t.Context(), "DROP TRIGGER fail_history_consume"); err != nil {
					t.Fatal(err)
				}
				// New service instance resumes from durable work, with no process memory.
				drainHistory(t, r)
				if row := readHistoryAggregate(t, r, "1d", r.at); row.TotalChecks != 1 || row.UpUS == 0 {
					t.Fatal("restart failed to recover work", row)
				}
			})
			t.Run("RevisionSurvivesConsumeReinsertABA", func(t *testing.T) {
				r := oldHistoryFixture(t, engine)
				r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
				old, err := r.f.projections.ListDirty(t.Context(), domain.DirtyResolution1m, 1)
				if err != nil || len(old) != 1 {
					t.Fatal(old, err)
				}
				if err := r.f.projections.ClearDirty(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				if err := r.f.projections.MarkDirty(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				if err := r.f.projections.ClearDirty(t.Context(), old); err != nil {
					t.Fatal(err)
				}
				current, err := r.f.projections.ListDirty(t.Context(), domain.DirtyResolution1m, 1)
				if err != nil || len(current) != 1 || current[0].Revision == old[0].Revision {
					t.Fatal("stale consumer erased reinserted work", current, err)
				}
			})
		})
	}
}

type blockedHistoryProjector struct {
	service *services.ProbeHistoryService
	entered chan struct{}
	release chan struct{}
}

func (p *blockedHistoryProjector) HistoryFreshness(m domain.Monitor) (time.Duration, error) {
	return p.service.HistoryFreshness(m)
}
func (p *blockedHistoryProjector) ProjectHistory(ctx context.Context, w domain.ProbeHistoryWork) (domain.ProbeHistoryProjection, error) {
	close(p.entered)
	select {
	case <-p.release:
		return p.service.ProjectHistory(ctx, w)
	case <-ctx.Done():
		return domain.ProbeHistoryProjection{}, ctx.Err()
	}
}

func TestProbeHistoryConcurrentRemarkDoesNotPublishStaleSnapshot(t *testing.T) {
	// MariaDB permits a source commit during the reader's repeatable-read snapshot.
	// SQLite instead serializes the source behind the writer lock (tested separately).
	r := oldHistoryFixture(t, "mariadb")
	r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets WHERE resolution <> '1m'"); err != nil {
		t.Fatal(err)
	}
	p := &blockedHistoryProjector{service: services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db)), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repository.NewRegionalCommitStore(r.f.db).ProcessHistoryWork(ctx, time.Now().UTC(), 1, p)
		done <- err
	}()
	select {
	case <-p.entered:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	second := r.ingest(t, r.batch(replayObservationAt(r, 2, 30, domain.StatusDown)))
	if second.AcceptedCount != 1 {
		t.Fatal(second)
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if replayCount(t, r.f, "heartbeat_1m") != 0 {
		t.Fatal("stale snapshot published after concurrent source mark")
	}
	drainHistory(t, r)
	row := readHistoryAggregate(t, r, "1m", r.at)
	if row.UpUS != 30*time.Second.Microseconds() || row.DownUS != 30*time.Second.Microseconds() {
		t.Fatalf("concurrent event disappeared: %+v", row)
	}
}

func TestProbeHistoryLegacyRollupsPreserveManagedCoverage(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
			drainHistory(t, r)
			repo := newEngineHeartbeatRepo(r.f)
			before := readHistoryAggregate(t, r, "1m", r.at)
			if err := repo.SaveAggregate1m(t.Context(), &ports.Aggregate1m{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Bucket: r.at, DownCount: 999, TotalChecks: 999}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SaveAggregate1h(t.Context(), &ports.Aggregate1h{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Bucket: r.at, DownCount: 999, TotalChecks: 999}); err != nil {
				t.Fatal(err)
			}
			if err := repo.SaveAggregate1d(t.Context(), &ports.Aggregate1d{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Bucket: r.at, DownCount: 999, TotalChecks: 999}); err != nil {
				t.Fatal(err)
			}
			for _, resolution := range []string{"1m", "1h", "1d"} {
				after := readHistoryAggregate(t, r, resolution, r.at)
				if !after.HistoryManaged || after.DownCount != 0 || after.TotalChecks != 1 || after.UpUS == 0 {
					t.Fatalf("legacy rollup overwrote %s coverage: %+v", resolution, after)
				}
			}
			if after := readHistoryAggregate(t, r, "1m", r.at); after.UpUS != before.UpUS {
				t.Fatal("minute duration changed", before, after)
			}
			rows, err := repo.GetAggregate1m(t.Context(), r.monitor, r.at)
			if err != nil || len(rows) == 0 || rows[0].Durations.Up == 0 {
				t.Fatal("duration coverage absent from repository read", rows, err)
			}
		})
	}
}

func TestProbeHistoryWindowReplacementPreservesBothSides(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			original := domain.MonitorHealthInterval{From: r.at, To: r.at.Add(5 * time.Minute), Status: domain.StatusUp, Policy: domain.HealthPolicyAnyDown, Counts: domain.ProbeHealthCounts{Assigned: 1, Up: 1}}
			if err := r.f.projections.ReplaceHealthHistory(t.Context(), r.monitor, original.From, original.To, []domain.MonitorHealthInterval{original}); err != nil {
				t.Fatal(err)
			}
			replacement := original
			replacement.From = r.at.Add(2 * time.Minute)
			replacement.To = r.at.Add(3 * time.Minute)
			replacement.Status = domain.StatusDown
			replacement.Counts.Up = 0
			replacement.Counts.Down = 1
			if err := r.f.projections.ReplaceHealthHistory(t.Context(), r.monitor, replacement.From, replacement.To, []domain.MonitorHealthInterval{replacement}); err != nil {
				t.Fatal(err)
			}
			rows, err := r.f.projections.ListHealthHistory(t.Context(), r.monitor, original.From, original.To)
			if err != nil || len(rows) != 3 || rows[0].Status != domain.StatusUp || rows[1].Status != domain.StatusDown || rows[2].Status != domain.StatusUp || !rows[0].From.Equal(original.From) || !rows[2].To.Equal(original.To) {
				t.Fatalf("replacement lost adjacent history: %+v %v", rows, err)
			}
		})
	}
}

func TestProbeHistorySQLiteSerializesSourceRemark(t *testing.T) {
	r := oldHistoryFixture(t, "sqlite")
	r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
	if _, err := r.f.db.ExecContext(t.Context(), "DELETE FROM probe_dirty_buckets WHERE resolution <> '1m'"); err != nil {
		t.Fatal(err)
	}
	secondDB, err := sqlite.NewDB(r.f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = secondDB.Close() }()
	second := repository.NewRegionalCommitStore(secondDB)
	p := &blockedHistoryProjector{service: services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db)), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := repository.NewRegionalCommitStore(r.f.db).ProcessHistoryWork(ctx, time.Now().UTC(), 1, p)
		done <- err
	}()
	select {
	case <-p.entered:
	case err := <-done:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	sourceStarted, sourceDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(sourceStarted)
		sourceDone <- second.MarkDirty(ctx, []domain.DirtyBucket{{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Resolution: domain.DirtyResolution1m, Bucket: r.at}})
	}()
	<-sourceStarted
	select {
	case err := <-sourceDone:
		t.Fatal("source escaped SQLite projection writer lock", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-sourceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	dirty, err := r.f.projections.ListDirty(t.Context(), domain.DirtyResolution1m, 100)
	found := false
	for _, row := range dirty {
		if row.Bucket.Equal(r.at) {
			found = true
		}
	}
	if err != nil || !found {
		t.Fatal("source mark lost after serialized projection", dirty, err)
	}
}

type historySelectionBarrier struct {
	entered, release chan struct{}
	fired            atomic.Bool
}

func (h *historySelectionBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}
func (h *historySelectionBarrier) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	if strings.Contains(event.Query, "CASE resolution") && h.fired.CompareAndSwap(false, true) {
		close(h.entered)
		select {
		case <-h.release:
		case <-ctx.Done():
		}
	}
}

func TestProbeHistoryParentRechecksChildrenAfterQueueSelection(t *testing.T) {
	for _, resolution := range []string{domain.DirtyResolution1h, domain.DirtyResolution1d} {
		t.Run(resolution, func(t *testing.T) {
			r := oldHistoryFixture(t, "mariadb")
			r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
			drainHistory(t, r)
			if err := r.f.projections.MarkDirty(t.Context(), []domain.DirtyBucket{{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Resolution: resolution, Bucket: r.at}}); err != nil {
				t.Fatal(err)
			}
			barrier := &historySelectionBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			r.f.db.AddQueryHook(barrier)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db)).ProcessBatch(ctx, time.Now().UTC(), 1)
				done <- err
			}()
			select {
			case <-barrier.entered:
			case err := <-done:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// Queue selection precedes the coherent read: fresh source commits both a
			// parent token and a dirty child before that transaction begins.
			r.ingest(t, r.batch(replayObservationAt(r, 2, 30, domain.StatusDown)))
			close(barrier.release)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			dirty, err := r.f.projections.ListDirty(t.Context(), resolution, 100)
			if err != nil || len(dirty) == 0 {
				t.Fatal("parent consumed new work before its dirty child was recomputed", dirty, err)
			}
			drainHistory(t, r)
			if parent := readHistoryAggregate(t, r, resolution, r.at); parent.DownUS == 0 {
				t.Fatal("parent never incorporated new child", parent)
			}
		})
	}
}

func TestProbeHistoryAssignmentRemovalClosesRegionalDenominator(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			if _, err := r.f.assignments.Replace(t.Context(), r.monitor, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown); err != nil {
				t.Fatal(err)
			}
			var boundary time.Time
			if err := r.f.db.NewRaw("SELECT ended_at FROM monitor_probe_assignment_history WHERE monitor_id = ? AND probe_id = ? AND ended_at IS NOT NULL", r.monitor, r.session.ProbeID).Scan(t.Context(), &boundary); err != nil {
				t.Fatal(err)
			}
			at := boundary.UTC().Truncate(time.Minute)
			svc := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db))
			for n := 0; n < 10; n++ {
				count, err := svc.ProcessBatch(t.Context(), at.Add(2*time.Minute), 100)
				if err != nil {
					t.Fatal(err)
				}
				if count == 0 {
					break
				}
			}
			old := readHistoryAggregate(t, r, "1m", at)
			if old.UnknownUS != boundary.Sub(at).Microseconds() || old.TotalChecks != 0 {
				t.Fatal("removed membership remained in denominator", old, boundary)
			}
			local := r
			local.session.ProbeID = domain.LocalProbeID
			added := readHistoryAggregate(t, local, "1m", at)
			if added.UnknownUS != at.Add(time.Minute).Sub(boundary).Microseconds() || added.TotalChecks != 0 {
				t.Fatal("new membership inherited evidence", added, boundary)
			}
		})
	}
}

func TestProbeHistoryLateClockRegressionRepairsBeyondFreshness(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp), replayObservationAt(r, 2, 3600, domain.StatusUp)))
			drainHistory(t, r)
			later := readHistoryAggregate(t, r, "1m", r.at.Add(time.Hour))
			if later.UpUS == 0 {
				t.Fatal("fixture did not establish later UP", later)
			}
			r.ingest(t, r.batch(replayObservationAt(r, 3, 30, domain.StatusDown)))
			drainHistory(t, r)
			later = readHistoryAggregate(t, r, "1m", r.at.Add(time.Hour))
			if later.UpUS != 0 || later.UnknownUS != time.Minute.Microseconds() {
				t.Fatal("earlier sequence regained authority beyond freshness", later)
			}
			reader := services.NewMonitorHealthService(newEngineMonitorRepo(r.f), r.f.assignments, r.f.commits, allowAllMonitors{})
			history, err := reader.History(t.Context(), 1, r.monitor, r.at.Add(time.Hour), r.at.Add(time.Hour+time.Minute))
			if err != nil || history.Durations.Up != 0 || history.Durations.Unknown != time.Minute {
				t.Fatal("on-demand reader resurrected earlier sequence", history, err)
			}
		})
	}
}

func TestProbeHistoryParentsPreserveLegacySampleStatistics(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			repo := newEngineHeartbeatRepo(r.f)
			if err := repo.SaveAggregate1m(t.Context(), &ports.Aggregate1m{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Bucket: r.at.Add(10 * time.Minute), UpCount: 3, TotalChecks: 3, PingCount: 3, AvgPing: 40, MinPing: 30, MaxPing: 50}); err != nil {
				t.Fatal(err)
			}
			r.ingest(t, r.batch(replayObservationAt(r, 1, 0, domain.StatusUp)))
			drainHistory(t, r)
			for _, resolution := range []string{"1h", "1d"} {
				row := readHistoryAggregate(t, r, resolution, r.at)
				if row.TotalChecks != 4 || row.PingCount != 4 || row.AvgPing != 35 {
					t.Fatalf("legacy counts disappeared at %s: %+v", resolution, row)
				}
			}
		})
	}
}
