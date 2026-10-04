package repository_test

import (
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestDeclarativeRestoreFleetAdmission(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			// An old worker already holds a real scheduler lease before import starts.
			leaseMonitors(t, f, "old-worker", 1)
			readiness := repository.NewHubWorkerReadinessStore(f.db)
			remote := f.remote(t, probeRegistryID1, "region")
			monitors := newEngineMonitorRepo(f)
			var tags ports.TagRepository
			if engine == "sqlite" {
				tags = sqlite.NewTagRepo(f.db)
			} else {
				tags = mariadb.NewTagRepo(f.db)
			}
			backup := services.NewBackupService(monitors, nil, nil, nil, tags, nil, nil, nil, nil, nil, nil, nil, nil)
			backup.SetProbeRegistry(f.registry)
			backup.SetProbeAssignments(f.assignments)
			backup.SetFleetActivationGate(services.NewFleetActivationGate(readiness, readinessLookback))
			doc := &services.BackupDocument{Version: services.BackupDocumentVersion,
				Probes:                  []services.BackupProbe{{ID: remote.ID, Key: remote.Key, Name: remote.Name, Kind: domain.ProbeKindRemote}},
				Monitors:                []services.BackupMonitor{{ID: 7, Name: "restored-region", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}},
				MonitorProbeAssignments: []services.BackupMonitorAssignmentSet{{MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown, Members: []services.BackupMonitorAssignmentMember{{ProbeKey: remote.Key}}}},
			}
			userID := f.user(t)
			result, err := backup.Import(t.Context(), userID, doc)
			if err != nil || result.MonitorsCreated != 0 || len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0].Reason, "hub workers") {
				t.Fatalf("unsafe restore: %+v %v", result, err)
			}
			var count int
			if err := f.db.NewRaw("SELECT COUNT(*) FROM monitors WHERE name = ?", "restored-region").Scan(t.Context(), &count); err != nil || count != 0 {
				t.Fatalf("refused restore left monitor: %d %v", count, err)
			}
			if err := f.db.NewRaw("SELECT COUNT(*) FROM monitor_probe_assignments WHERE probe_id = ?", remote.ID).Scan(t.Context(), &count); err != nil || count != 0 {
				t.Fatalf("refused restore left remote membership: %d %v", count, err)
			}
			// Once the same worker attests, the same document may activate remotely.
			if err := readiness.DeclareWorker(t.Context(), "old-worker", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			result, err = backup.Import(t.Context(), userID, doc)
			if err != nil || result.MonitorsCreated != 1 || len(result.Skipped) != 0 {
				t.Fatalf("aware restore: %+v %v", result, err)
			}
			if err := f.db.NewRaw("SELECT COUNT(*) FROM monitor_probe_assignments WHERE probe_id = ?", remote.ID).Scan(t.Context(), &count); err != nil || count != 1 {
				t.Fatalf("remote membership not committed: %d %v", count, err)
			}
		})
	}
}
