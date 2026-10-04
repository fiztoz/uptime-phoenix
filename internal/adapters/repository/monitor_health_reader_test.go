package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type allowAllMonitors struct{}

func (allowAllMonitors) CanViewMonitor(context.Context, int64, int64) (bool, error) { return true, nil }

func TestMonitorHealthReaderUsesPersistedRegionalState(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "asia")
			if _, err := f.assignments.Replace(ctx, monitorID, 1, []string{"local", remote.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Second)
			local := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusUp, 0, now)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: local, State: stateFrom(local)}); err != nil {
				t.Fatal(err)
			}
			var previous *domain.RetryState
			for seq := int64(1); seq <= 2; seq++ {
				eval := services.EvaluateRetry(previous, domain.StatusDown, 1)
				sample := regionalSample(monitorID, remote.ID, remoteStreamID, 1, seq, eval.State.Status, eval.State.DownCount, now)
				sample.RawStatus = domain.StatusDown
				if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: sample, State: stateFrom(sample)}); err != nil {
					t.Fatal(err)
				}
				previous = &eval.State
			}

			otherID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, otherID); err != nil {
				t.Fatal(err)
			}
			other := regionalSample(otherID, "local", localStreamID, 1, 1, domain.StatusUp, 0, now)
			other.StreamID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: other, State: stateFrom(other)}); err != nil {
				t.Fatal(err)
			}

			states, err := f.commits.ListStates(ctx, monitorID)
			if err != nil || len(states) != 2 {
				t.Fatalf("list states: %d %v", len(states), err)
			}
			seen := map[string]bool{}
			for _, state := range states {
				if state.MonitorID != monitorID {
					t.Fatal("ListStates leaked another monitor")
				}
				seen[state.ProbeID] = true
			}
			if !seen[domain.LocalProbeID] || !seen[remote.ID] {
				t.Fatalf("expected local and remote state, got %+v", states)
			}

			zone := time.FixedZone("UTC+7", 7*3600)
			rows, err := f.commits.ListObservations(ctx, monitorID, "local", now.In(zone).Add(-time.Second), now.In(zone).Add(time.Second))
			if err != nil || len(rows) != 1 || rows[0].Status != domain.StatusUp {
				t.Fatalf("utc window: %+v %v", rows, err)
			}

			svc := services.NewMonitorHealthService(newEngineMonitorRepo(f), f.assignments, f.commits, allowAllMonitors{})
			got, err := svc.Current(ctx, 1, monitorID, now)
			if err != nil || got.Health.Status != domain.StatusDown || got.Policy != domain.HealthPolicyAnyDown {
				t.Fatalf("any_down: %+v %v", got, err)
			}

			if _, err := f.assignments.Replace(ctx, monitorID, 2, []string{"local", remote.ID}, domain.HealthPolicyAllDown); err != nil {
				t.Fatal(err)
			}
			got, err = svc.Current(ctx, 1, monitorID, now)
			if err != nil || got.Health.Status != domain.StatusUp {
				t.Fatalf("all_down: %+v %v", got, err)
			}
		})
	}
}
