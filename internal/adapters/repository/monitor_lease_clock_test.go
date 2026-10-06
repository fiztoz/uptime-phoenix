package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Lock-wait clock contract (issue #63 follow-up): lease expiry must be
// evaluated with a clock read AFTER the engine's monitor lock is held. A "now"
// sampled before a blocking lock wait is stale by the time the row is ours, so
// a lease that expired during the wait reads as still valid — reviving it (or
// keeping its instance) exactly when it must instead become a NEW lease
// instance that rejects the previous instance's queued work.
//
// The tests below hold the engine's monitor lock from a second connection
// while the real TTL elapses, which is the only deterministic way to make the
// wait itself change the expiry verdict (backdating cannot: the row is locked).

const lockWaitTTL = 600 * time.Millisecond

// holdMonitorLock blocks monitor lease writers from a second connection until
// release runs. The no-op write holds the engine's write/row lock the same way
// a slow claim, refresh, or commit transaction would.
func holdMonitorLock(t *testing.T, f probeRegistryFixture, monitorID int64) (release func()) {
	t.Helper()
	var db *bun.DB
	var err error
	if f.engine == "sqlite" {
		db, err = sqlite.NewDB(f.dsn)
	} else {
		db, err = mariadb.NewDB(f.dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(context.Background(), "UPDATE monitors SET leased_at = leased_at WHERE id = ?", monitorID); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		_ = db.Close()
	}
	t.Cleanup(release)
	return release
}

// TestClaimBatchReacquiresLeaseExpiringDuringLockWait: the own-worker renewal
// decision must see the lease as expired when the row lock is finally acquired,
// not when ClaimBatch was entered. A lease that expires while the claim waits
// is a same-worker REACQUISITION — a new epoch — or pre-expiry queued work
// keeps its authority.
func TestClaimBatchReacquiresLeaseExpiringDuringLockWait(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := f.monitor(t)
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, lockWaitTTL); err != nil {
				t.Fatal(err)
			}
			if got := leaseEpochOf(t, f, id); got != 1 {
				t.Fatalf("fresh claim epoch = %d, want 1", got)
			}

			release := holdMonitorLock(t, f, id)
			done := make(chan error, 1)
			go func() {
				// Entered while the lease is still valid; it expires during the
				// lock wait below.
				_, err := repo.ClaimBatch(ctx, "worker-a", 1000, lockWaitTTL)
				done <- err
			}()
			time.Sleep(3 * lockWaitTTL)
			release()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got := leaseEpochOf(t, f, id); got != 2 {
				t.Fatalf("lease expiring during claim lock wait kept instance %d, want 2", got)
			}
		})
	}
}

// TestRefreshLeaseSkipsLeaseExpiringDuringLockWait: the TTL predicate itself
// must be evaluated with the post-lock clock, or a lease that expires while
// the refresh waits is revived in place — no epoch bump — and its pre-expiry
// queued work keeps authority.
func TestRefreshLeaseSkipsLeaseExpiringDuringLockWait(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := f.monitor(t)
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, lockWaitTTL); err != nil {
				t.Fatal(err)
			}

			type refreshResult struct {
				n   int64
				err error
			}
			release := holdMonitorLock(t, f, id)
			done := make(chan refreshResult, 1)
			go func() {
				n, err := repo.RefreshLease(ctx, "worker-a", lockWaitTTL)
				done <- refreshResult{n: n, err: err}
			}()
			time.Sleep(3 * lockWaitTTL)
			release()
			res := <-done
			if res.err != nil {
				t.Fatal(res.err)
			}
			if res.n != 0 {
				t.Fatalf("refresh revived %d lease(s) that expired during its lock wait", res.n)
			}
			if got := leaseEpochOf(t, f, id); got != 1 {
				t.Fatalf("expired lease instance moved to %d without a claim", got)
			}
			// The untouched lease reacquires as a new instance afterwards.
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, lockWaitTTL); err != nil {
				t.Fatal(err)
			}
			if got := leaseEpochOf(t, f, id); got != 2 {
				t.Fatalf("claim after refresh kept instance %d, want 2", got)
			}
		})
	}
}

// TestLocalHeartbeatCommitEvaluatesExpiryAfterLockWait guards the commit-side
// clock: expiry is re-evaluated after the monitor row lock is acquired, so a
// lease that expires while the commit waits is rejected instead of being
// honored from an entry-time reading.
func TestLocalHeartbeatCommitEvaluatesExpiryAfterLockWait(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := localMonitor(t, f)
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, lockWaitTTL); err != nil {
				t.Fatal(err)
			}
			// Build the payload before the lock is held: fixture setup writes
			// would themselves block behind it.
			commit := fencedDownCommit(t, f, id, &domain.LeaseFence{
				WorkerID: "worker-a", LeaseEpoch: leaseEpochOf(t, f, id), LeaseTTL: lockWaitTTL,
			})

			release := holdMonitorLock(t, f, id)
			done := make(chan error, 1)
			go func() {
				_, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, commit)
				done <- err
			}()
			time.Sleep(3 * lockWaitTTL)
			release()
			err := <-done
			if !errors.Is(err, ports.ErrStaleLease) {
				t.Fatalf("commit under a lease that expired during its lock wait: err=%v", err)
			}
			if seq := localSequence(t, f); seq != 0 {
				t.Fatalf("rejected commit consumed sequence %d", seq)
			}
		})
	}
}
