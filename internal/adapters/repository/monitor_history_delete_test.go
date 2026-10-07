package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMonitorDeleteRemovesHistoricalHeartbeats(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			if result := r.ingest(t, r.batch(r.observation(1))); result.AcceptedCount != 1 {
				t.Fatalf("seed replay: %+v", result)
			}
			other := r.f.monitor(t)
			// Legacy local rows and retired remote generations span different partitions.
			for _, row := range []struct {
				id         int64
				probe      string
				generation int64
				at         time.Time
			}{
				{r.monitor, "local", 1, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
				{r.monitor, r.session.ProbeID, 7, time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
				{r.monitor, r.session.ProbeID, 8, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
				{other, "local", 1, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)},
			} {
				if _, err := r.f.db.ExecContext(ctx, "INSERT INTO heartbeats (monitor_id, probe_id, assignment_generation, status, time) VALUES (?, ?, ?, 1, ?)", row.id, row.probe, row.generation, row.at); err != nil {
					t.Fatal(err)
				}
			}
			count := func(id int64) int64 {
				t.Helper()
				n, err := r.f.db.NewSelect().Table("heartbeats").Where("monitor_id = ?", id).Count(ctx)
				if err != nil {
					t.Fatal(err)
				}
				return n
			}
			if count(r.monitor) != 3 {
				t.Fatal("missing historical fixture")
			}
			if engine == "mariadb" {
				n, err := r.f.db.NewSelect().TableExpr("information_schema.PARTITIONS").Where("TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'heartbeats' AND PARTITION_NAME IS NOT NULL").Count(ctx)
				if err != nil || n < 2 {
					t.Fatalf("requires production partitioned schema: %d %v", n, err)
				}
			}
			repo := newEngineMonitorRepo(r.f)
			// A cleanup error must not commit the parent deletion or any cascade.
			trigger := "CREATE TRIGGER fail_history_delete BEFORE DELETE ON heartbeats BEGIN SELECT RAISE(ABORT, 'injected history cleanup failure'); END"
			if engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_history_delete BEFORE DELETE ON heartbeats FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected history cleanup failure'"
			}
			if _, err := r.f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, _ = r.f.db.ExecContext(cleanupCtx, "DROP TRIGGER IF EXISTS fail_history_delete")
			})
			if err := repo.Delete(ctx, r.monitor); err == nil {
				t.Fatal("history cleanup failure reported success")
			}
			if _, err := repo.GetByID(ctx, r.monitor); err != nil || count(r.monitor) != 3 {
				t.Fatalf("failed deletion was not atomic: %v", err)
			}
			if _, err := r.f.db.ExecContext(ctx, "DROP TRIGGER fail_history_delete"); err != nil {
				t.Fatal(err)
			}
			if err := repo.Delete(ctx, r.monitor); err != nil {
				t.Fatal(err)
			}
			if _, err := repo.GetByID(ctx, r.monitor); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("deleted monitor lookup: %v", err)
			}
			if replayCount(t, r.f, "probe_observations") != 0 {
				t.Fatal("regional history was not cascaded")
			}
			if count(r.monitor) != 0 || count(other) != 1 {
				t.Fatal("history was retained or another monitor was affected")
			}
			if result := r.ingest(t, r.batch(r.observation(2))); len(result.Rejected) != 1 || result.Rejected[0].Code != "monitor_not_found" {
				t.Fatalf("deleted monitor replay: %+v", result)
			}
			if count(r.monitor) != 0 {
				t.Fatal("replay resurrected history")
			}
			if err := repo.Delete(ctx, r.monitor); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("repeated delete: %v", err)
			}
		})
	}
}
