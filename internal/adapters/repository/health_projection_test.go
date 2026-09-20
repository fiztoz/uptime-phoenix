package repository_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestRegionalCommitMarksDirtyBucketsAndProjectsOverallHistory(t *testing.T) {
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
			observed := time.Now().UTC().Truncate(time.Minute)
			local := regionalSample(monitorID, "local", localStreamID, 1, 1, domain.StatusUp, 0, observed)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: local, State: stateFrom(local)}); err != nil {
				t.Fatal(err)
			}
			again := regionalSample(monitorID, "local", localStreamID, 1, 2, domain.StatusUp, 0, observed.Add(time.Second))
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: again, State: stateFrom(again)}); err != nil {
				t.Fatal(err)
			}
			remoteObs := regionalSample(monitorID, remote.ID, remoteStreamID, 1, 1, domain.StatusDown, 1, observed)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: remoteObs, State: stateFrom(remoteObs)}); err != nil {
				t.Fatal(err)
			}

			dirty, err := f.projections.ListDirty(ctx, domain.DirtyResolutionOverall, 20)
			if err != nil {
				t.Fatal(err)
			}
			if len(dirty) != 1 {
				t.Fatalf("overall dirty coalescing: %+v", dirty)
			}
			for _, bucket := range dirty {
				if bucket.MonitorID != monitorID || bucket.Bucket.Location() != time.UTC {
					t.Fatalf("dirty identity: %+v", bucket)
				}
			}
			oneMinute, err := f.projections.ListDirty(ctx, domain.DirtyResolution1m, 20)
			if err != nil || len(oneMinute) != 2 {
				t.Fatalf("1m dirty: %+v %v", oneMinute, err)
			}

			otherID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, otherID); err != nil {
				t.Fatal(err)
			}
			other := regionalSample(otherID, "local", "dddddddd-dddd-4ddd-8ddd-dddddddddddd", 1, 1, domain.StatusUp, 0, observed)
			if err := f.commits.Commit(ctx, domain.RegionalCommit{Observation: other, State: stateFrom(other)}); err != nil {
				t.Fatal(err)
			}
			rows, err := f.commits.ListObservationsInRange(ctx, monitorID, observed.Add(-time.Second), observed.Add(time.Second))
			if err != nil || len(rows) != 3 {
				t.Fatalf("range: %d %v", len(rows), err)
			}
			for _, row := range rows {
				if row.MonitorID != monitorID {
					t.Fatal("ListObservationsInRange leaked another monitor")
				}
			}

			svc := services.NewMonitorHealthService(newEngineMonitorRepo(f), f.assignments, f.commits, allowAllMonitors{})
			svc.SetProjections(f.projections)
			if err := svc.ProjectCurrent(ctx, monitorID, observed); err != nil {
				t.Fatal(err)
			}
			state, err := f.projections.GetHealthState(ctx, monitorID)
			if err != nil || state.Status != domain.StatusDown || state.ProjectionVersion != 1 || state.AsOf.Location() != time.UTC {
				t.Fatalf("current projection: %+v %v", state, err)
			}

			processed, err := svc.ProcessDirty(ctx, observed.Truncate(time.Minute).Add(2*time.Minute), 10)
			if err != nil || processed < 1 {
				t.Fatalf("process dirty: %d %v", processed, err)
			}
			history, err := f.projections.ListHealthHistory(ctx, monitorID, observed.Truncate(time.Minute), observed.Truncate(time.Minute).Add(time.Minute))
			if err != nil || len(history) == 0 {
				t.Fatalf("overall history: %+v %v", history, err)
			}
			down := false
			for _, interval := range history {
				if interval.From.Location() != time.UTC || interval.To.Location() != time.UTC {
					t.Fatal("history times must be UTC")
				}
				if interval.Status == domain.StatusDown {
					down = true
				}
			}
			if !down {
				t.Fatalf("overall history missing DOWN: %+v", history)
			}

			if err := runNamedMigration(t, f, "039_probe_health_projection", "down"); err == nil {
				t.Fatal("downgrade discarded overall projections")
			}
		})
	}
}

func runNamedMigration(t *testing.T, f probeRegistryFixture, name, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.engine, "migrations", name+"."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}
