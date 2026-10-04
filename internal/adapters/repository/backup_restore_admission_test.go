package repository_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type inspectRestoringMonitor struct {
	ports.MonitorRepository
	inspect func(context.Context, int64)
}

func (r inspectRestoringMonitor) Create(ctx context.Context, m *domain.Monitor) error {
	if err := r.MonitorRepository.Create(ctx, m); err != nil {
		return err
	}
	r.inspect(ctx, m.ID)
	return nil
}

func TestBackupRestoreSchedulerAdmission(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			userID := f.user(t)
			var base ports.MonitorRepository
			var tags ports.TagRepository
			if engine == "sqlite" {
				base = sqlite.NewMonitorRepo(f.db)
				tags = sqlite.NewTagRepo(f.db)
			} else {
				base = mariadb.NewMonitorRepo(f.db)
				tags = mariadb.NewTagRepo(f.db)
			}
			var createdID int64
			repo := inspectRestoringMonitor{MonitorRepository: base, inspect: func(ctx context.Context, id int64) {
				createdID = id
				stored, err := base.GetByID(ctx, id)
				if err != nil || stored.Active {
					t.Fatalf("created restore must be inactive: %+v %v", stored, err)
				}
				// These are the production scheduler's admission reads, made after
				// the CREATE transaction committed and before assignment restore.
				allowed, err := f.assignments.ExecutableByLocal(ctx, []int64{id})
				if err != nil || allowed[id] != 1 {
					t.Fatalf("fixture has no placeholder assignment: %v %v", allowed, err)
				}
				active, err := base.ListActive(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for _, m := range active {
					if m.ID == id {
						t.Fatal("scheduler admitted remote restore through local placeholder")
					}
				}
			}}
			bus := eventbus.NewMemoryBus()
			defer bus.Close()
			backup := services.NewBackupService(repo, nil, nil, nil, tags, nil, nil, nil, nil, nil, nil, nil, nil)
			backup.SetMonitorService(services.NewMonitorService(repo, bus))
			backup.SetProbeRegistry(f.registry)
			backup.SetProbeAssignments(f.assignments)
			doc := &services.BackupDocument{Version: services.BackupDocumentVersion,
				Probes:                  []services.BackupProbe{{ID: probeRegistryID1, Key: "region", Name: "Region", Kind: domain.ProbeKindRemote}},
				Monitors:                []services.BackupMonitor{{ID: 17, Name: "restored", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "http://example.test"}}},
				MonitorProbeAssignments: []services.BackupMonitorAssignmentSet{{MonitorID: 17, HealthPolicy: domain.HealthPolicyAnyDown, Members: []services.BackupMonitorAssignmentMember{{ProbeKey: "region"}}}},
			}
			summary, err := backup.Import(t.Context(), userID, doc)
			if err != nil || summary.MonitorsCreated != 1 || len(summary.Skipped) != 0 {
				t.Fatalf("restore: %+v %v", summary, err)
			}
			stored, err := base.GetByID(t.Context(), createdID)
			if err != nil || !stored.Active {
				t.Fatalf("final activation: %+v %v", stored, err)
			}
			allowed, err := f.assignments.ExecutableByLocal(t.Context(), []int64{createdID})
			if err != nil || len(allowed) != 0 {
				t.Fatalf("completed restore became local: %v %v", allowed, err)
			}
		})
	}
}
