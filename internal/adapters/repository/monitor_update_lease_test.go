package repository_test

import (
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMonitorUpdatePreservesConcurrentWorkerLease(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			id := localMonitor(t, f)
			if _, err := f.db.NewUpdate().Table("monitors").Set("user_id = ?", f.user(t)).Where("id = ?", id).Exec(t.Context()); err != nil {
				t.Fatal(err)
			}
			var monitors ports.MonitorRepository
			if engine == "sqlite" {
				monitors = sqlite.NewMonitorRepo(f.db)
			} else {
				monitors = mariadb.NewMonitorRepo(f.db)
			}
			ctx := t.Context()
			stale, err := monitors.GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := monitors.ClaimBatch(ctx, "new-worker", 1, time.Minute)
			if err != nil || len(claimed) != 1 || claimed[0].ID != id {
				t.Fatalf("claim: rows=%d err=%v", len(claimed), err)
			}
			current := new(repository.MonitorModel)
			if err := f.db.NewSelect().Model(current).Where("id = ?", id).Scan(ctx); err != nil {
				t.Fatal(err)
			}
			stale.Name = "Edited after claim"
			if err := monitors.Update(ctx, stale); err != nil {
				t.Fatal(err)
			}
			got, err := monitors.GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != stale.Name {
				t.Fatal("monitor edit did not persist")
			}
			stored := new(repository.MonitorModel)
			if err := f.db.NewSelect().Model(stored).Where("id = ?", id).Scan(ctx); err != nil {
				t.Fatal(err)
			}
			if stored.WorkerID == nil || current.WorkerID == nil || *stored.WorkerID != *current.WorkerID || stored.LeasedAt == nil || current.LeasedAt == nil || !stored.LeasedAt.Equal(*current.LeasedAt) {
				t.Fatal("stale monitor edit overwrote a committed worker lease")
			}
		})
	}
}
