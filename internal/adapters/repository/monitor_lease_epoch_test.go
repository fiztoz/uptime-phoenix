package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Lease-instance ownership contract (issue #63): the epoch changes only when a
// lease is newly established — fresh claim, takeover, or same-worker
// reacquisition after expiry — and never on renewals. The fence captured at
// queue time is only valid while the epoch is unchanged, so a bump here is
// exactly what rejects older queued work.
func TestMonitorLeaseEpochLifecycle(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := f.monitor(t)
			// Full-row Update needs a real owner: fixture monitors are inserted
			// raw without user_id and a stale-model write would violate the FK.
			if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET user_id = ? WHERE id = ?", f.user(t), id); err != nil {
				t.Fatal(err)
			}
			repo := newEngineMonitorRepo(f)
			claim := func(worker string) {
				t.Helper()
				if _, err := repo.ClaimBatch(ctx, worker, 1000, fenceTTL); err != nil {
					t.Fatalf("claim as %s: %v", worker, err)
				}
			}
			expectEpoch := func(want int64, why string) {
				t.Helper()
				if got := leaseEpochOf(t, f, id); got != want {
					t.Fatalf("%s: epoch = %d, want %d", why, got, want)
				}
			}

			expectEpoch(0, "fresh row")
			claim("worker-a")
			expectEpoch(1, "fresh claim")

			// Renewals never move the instance. Make the stamp observably
			// older: MariaDB reports zero changed rows for same-second writes.
			if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET leased_at = ? WHERE id = ?", time.Now().UTC().Add(-fenceTTL/2), id); err != nil {
				t.Fatal(err)
			}
			if n, err := repo.RefreshLease(ctx, "worker-a", fenceTTL); err != nil || n != 1 {
				t.Fatalf("refresh live lease: n=%d err=%v", n, err)
			}
			expectEpoch(1, "refresh lease")
			claim("worker-a") // own-worker arm while the lease is still valid
			expectEpoch(1, "own-worker re-stamp")

			// A refresh never revives an expired lease: leaving the row
			// untouched is exactly what forces the claim below to establish a
			// NEW instance instead of re-stamping this one in place.
			expireLease(t, f, id)
			if n, err := repo.RefreshLease(ctx, "worker-a", fenceTTL); err != nil || n != 0 {
				t.Fatalf("refresh expired lease: n=%d err=%v", n, err)
			}
			expectEpoch(1, "expired lease is not extended by refresh")

			// Same-worker reacquisition after expiry is a new instance.
			claim("worker-a")
			expectEpoch(2, "same-worker reacquisition after expiry")

			// Takeover is a new instance.
			expireLease(t, f, id)
			claim("worker-b")
			expectEpoch(3, "takeover")

			// Release ends the instance; the next claim creates one.
			if _, err := repo.ReleaseLeases(ctx, "worker-b"); err != nil {
				t.Fatal(err)
			}
			expectEpoch(3, "release keeps the historical value")
			claim("worker-a")
			expectEpoch(4, "fresh claim after release")

			// Monitor edits never write lease columns (issue #63 contract:
			// Update must ExcludeColumn the epoch alongside worker_id/leased_at).
			stale, err := repo.GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			stale.Name = "Edited after claim"
			if err := repo.Update(ctx, stale); err != nil {
				t.Fatal(err)
			}
			expectEpoch(4, "monitor edit")
			updated, err := repo.GetByID(ctx, id)
			if err != nil || updated.Name != stale.Name {
				t.Fatalf("monitor edit did not persist: %+v %v", updated, err)
			}
		})
	}
}

// TestMonitorLeaseEpochSurvivesListByWorker is the regression for "ListByWorker
// drops leases": the scheduler fences queued work on the returned identity, so
// a read that discards it silently disarms commit-side fencing.
func TestMonitorLeaseEpochSurvivesListByWorker(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := f.monitor(t)
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, fenceTTL); err != nil {
				t.Fatal(err)
			}
			// Move the instance once so a dropped epoch would be visible as 0.
			expireLease(t, f, id)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, fenceTTL); err != nil {
				t.Fatal(err)
			}
			leases, err := repo.(interface {
				ListByWorker(ctx context.Context, workerID string, leaseExpiry time.Time) ([]*domain.LeasedMonitor, error)
			}).ListByWorker(ctx, "worker-a", time.Now().UTC().Add(-fenceTTL))
			if err != nil {
				t.Fatal(err)
			}
			if len(leases) != 1 {
				t.Fatalf("leased monitors = %d", len(leases))
			}
			lease := leases[0]
			if lease.Monitor.ID != id || lease.WorkerID != "worker-a" || lease.LeaseEpoch != 2 || lease.LeasedAt.IsZero() {
				t.Fatalf("lease identity dropped: %+v", lease)
			}
		})
	}
}
