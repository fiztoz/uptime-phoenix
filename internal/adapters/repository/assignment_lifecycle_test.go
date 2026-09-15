package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMonitorCreateInitializesLocalAssignment(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitors := newEngineMonitorRepo(f)
			m := &domain.Monitor{UserID: f.user(t), Name: "created", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			if err := monitors.Create(ctx, m); err != nil || m.ID == 0 {
				t.Fatalf("create: %+v %v", m, err)
			}
			set, err := f.assignments.GetByMonitorID(ctx, m.ID)
			if err != nil || set.Revision != 1 || !domain.LocalWorkerMayRun(set) {
				t.Fatalf("assignment: %+v %v", set, err)
			}
			allowed, err := f.assignments.ExecutableByLocal(ctx, []int64{m.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := allowed[m.ID]; !ok {
				t.Fatal("created monitor is not locally executable")
			}
		})
	}
}

func TestClaimBatchSkipsRemoteOnlyAndKeepsLegacy(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitors := newEngineMonitorRepo(f)
			owner := f.user(t)
			local := &domain.Monitor{UserID: owner, Name: "local-run", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			if err := monitors.Create(ctx, local); err != nil {
				t.Fatal(err)
			}
			legacyID := f.monitor(t)
			_, _ = f.db.ExecContext(ctx, "UPDATE monitors SET active = 1 WHERE id = ?", legacyID)
			remoteOnly := &domain.Monitor{UserID: owner, Name: "remote-only", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			if err := monitors.Create(ctx, remoteOnly); err != nil {
				t.Fatal(err)
			}
			probe := f.remote(t, probeRegistryID1, "asia")
			set, err := f.assignments.GetByMonitorID(ctx, remoteOnly.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.assignments.Replace(ctx, remoteOnly.ID, set.Revision, []string{probe.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			claimed, err := monitors.ClaimBatch(ctx, "worker-1", 50, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			seen := map[int64]bool{}
			for _, m := range claimed {
				seen[m.ID] = true
			}
			if !seen[local.ID] || !seen[legacyID] {
				t.Fatalf("expected local and legacy monitors claimed, got %+v", seen)
			}
			if seen[remoteOnly.ID] {
				t.Fatal("hub worker claimed a remote-only monitor")
			}
		})
	}
}

func newEngineMonitorRepo(f probeRegistryFixture) ports.MonitorRepository {
	if f.engine == "sqlite" {
		return sqlite.NewMonitorRepo(f.db)
	}
	return mariadb.NewMonitorRepo(f.db)
}
