package repository_test

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestRegionalRecoveryResolvesPublicIncident(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, path := range []string{"replay", "snapshot"} {
				t.Run(path, func(t *testing.T) {
					r := newReplayFixture(t, engine)
					activateReplayConfig(t, r)
					var repos repositorySet
					var incidents ports.IncidentRepository
					if engine == "sqlite" {
						repos = sqliteRepositorySet(sqlite.NewRepository(r.f.db))
						incidents = sqlite.NewIncidentRepo(r.f.db)
					} else {
						repos = mariadbRepositorySet(mariadb.NewRepository(r.f.db))
						incidents = mariadb.NewIncidentRepo(r.f.db)
					}
					ctx := t.Context()
					page := &domain.StatusPage{Slug: "regional-recovery", Title: "Regional", Published: true, AutoResolveIncidents: true}
					if err := repos.statusPages.Create(ctx, page); err != nil {
						t.Fatal(err)
					}
					if err := repos.statusPageMonitors.AddMonitor(ctx, page.ID, r.monitor, 1); err != nil {
						t.Fatal(err)
					}
					incident := &domain.Incident{StatusPageID: page.ID, Title: "Outage", Style: "danger", Active: true}
					if err := incidents.Create(ctx, incident); err != nil {
						t.Fatal(err)
					}
					health := services.NewMonitorHealthService(repos.monitors, r.f.assignments, r.f.commits, nil)
					pages := services.NewStatusPageService(repos.statusPages, incidents, nil, repos.statusPageMonitors, repos.monitors, repos.heartbeats, nil)
					pages.SetAggregateStatus(health)
					replay, err := services.NewProbeReplayService(r.store, &services.AccessService{})
					if err != nil {
						t.Fatal(err)
					}
					state, err := services.NewProbeStateService(r.store, &services.AccessService{})
					if err != nil {
						t.Fatal(err)
					}
					replay.SetStatusPageRecovery(health, pages)
					state.SetStatusPageRecovery(health, pages)
					for n, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
						seq := int64(n + 1)
						if path == "replay" {
							event := r.observation(seq)
							event.Observation.Status, event.Observation.RawStatus = status, status
							if status == domain.StatusDown {
								event.Observation.DownCount = 1
							}
							if _, err := replay.ProcessBatch(ctx, r.session, r.batch(event)); err != nil {
								t.Fatal(err)
							}
						} else {
							if _, err := state.ApplySnapshot(ctx, r.session, currentSnapshot(r, seq, currentEntry(r, seq, status))); err != nil {
								t.Fatal(err)
							}
						}
						got, err := incidents.GetByID(ctx, incident.ID)
						if err != nil {
							t.Fatal(err)
						}
						if got.Active != (status == domain.StatusDown) {
							t.Fatalf("incident active=%v after committed remote %v", got.Active, status)
						}
					}
				})
			}
		})
	}
}
