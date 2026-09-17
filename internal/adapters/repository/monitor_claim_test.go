package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestWorkerMonitorReaderScopesCurrentLeases(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			cutoff := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
			var want int64
			for _, tc := range []struct {
				worker string
				at     *time.Time
				active bool
			}{
				{"worker-a", &cutoff, true},
				{"worker-b", &cutoff, true},
				{"worker-a", timePointer(cutoff.Add(-time.Second)), true},
				{"worker-a", &cutoff, false},
				{"worker-a", nil, true},
			} {
				id := f.monitor(t)
				if want == 0 {
					want = id
				}
				if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET worker_id = ?, leased_at = ?, active = ? WHERE id = ?", tc.worker, tc.at, tc.active, id); err != nil {
					t.Fatal(err)
				}
			}
			reader := newEngineMonitorRepo(f).(ports.WorkerMonitorReader)
			// A non-UTC caller must query the same UTC wall-clock boundary.
			got, err := reader.ListByWorker(ctx, "worker-a", cutoff.In(time.FixedZone("UTC+7", 7*60*60)))
			if err != nil || len(got) != 1 || got[0].ID != want {
				t.Fatalf("current leases: %+v, err=%v", got, err)
			}
		})
	}
}

func timePointer(at time.Time) *time.Time { return &at }

func TestClaimBatchReturnsOnlyItsBoundedClaim(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			repo := newEngineMonitorRepo(f)
			owner := f.user(t)
			for _, name := range []string{"first", "second"} {
				if err := repo.Create(ctx, &domain.Monitor{UserID: owner, Name: name, Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}); err != nil {
					t.Fatal(err)
				}
			}
			first, err := repo.ClaimBatch(ctx, "first-worker", 1, time.Minute)
			if err != nil || len(first) != 1 {
				var stored int
				if countErr := f.db.NewSelect().TableExpr("monitors").ColumnExpr("COUNT(*)").Where("worker_id = ?", "first-worker").Scan(ctx, &stored); countErr != nil {
					t.Fatal(countErr)
				}
				t.Fatalf("claim returned %d monitors, stored %d leases, err=%v", len(first), stored, err)
			}
			second, err := repo.ClaimBatch(ctx, "second-worker", 1, time.Minute)
			if err != nil || len(second) != 1 || first[0].ID == second[0].ID {
				t.Fatalf("second worker: %+v err=%v", second, err)
			}
			// One owner may already hold several rows with the same stored time.
			// Reclaiming a batch of one must never return every such row.
			if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET worker_id = ?, leased_at = ?", "first-worker", time.Now().UTC().Truncate(time.Second)); err != nil {
				t.Fatal(err)
			}
			again, err := repo.ClaimBatch(ctx, "first-worker", 1, time.Minute)
			if err != nil || len(again) != 1 || again[0].ID != first[0].ID {
				t.Fatalf("bounded reclaim: %+v err=%v", again, err)
			}
		})
	}
}

func TestMariaDBLeaseTimestampPrecisionControl(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	ctx := context.Background()
	id := f.monitor(t)
	whole := time.Now().UTC().Truncate(time.Second)
	fractional := whole.Add(123456 * time.Microsecond)
	if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET worker_id = ?, leased_at = ? WHERE id = ?", "precision-control", fractional, id); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		at   time.Time
		want int
	}{{fractional, 0}, {whole, 1}} {
		var count int
		if err := f.db.NewSelect().TableExpr("monitors").ColumnExpr("COUNT(*)").Where("id = ? AND worker_id = ? AND leased_at = ?", id, "precision-control", tc.at).Scan(ctx, &count); err != nil || count != tc.want {
			t.Fatalf("stored timestamp equality at %s: got %d want %d err=%v", tc.at, count, tc.want, err)
		}
	}
}

func TestClaimBatchConcurrentWorkers(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			repo := newEngineMonitorRepo(f)
			f.monitor(t)
			f.monitor(t)
			type result struct {
				monitors []*domain.Monitor
				err      error
			}
			start := make(chan struct{})
			results := make(chan result, 2)
			for _, worker := range []string{"worker-a", "worker-b"} {
				go func() {
					<-start
					monitors, err := repo.ClaimBatch(ctx, worker, 1, time.Minute)
					results <- result{monitors, err}
				}()
			}
			close(start)
			seen := make(map[int64]bool)
			for range 2 {
				got := <-results
				if got.err != nil || len(got.monitors) != 1 {
					t.Errorf("concurrent claim: %+v, err=%v", got.monitors, got.err)
					continue
				}
				if seen[got.monitors[0].ID] {
					t.Errorf("two owners claimed monitor %d", got.monitors[0].ID)
				}
				seen[got.monitors[0].ID] = true
			}
		})
	}
}
