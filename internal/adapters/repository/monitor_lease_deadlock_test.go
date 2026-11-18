package repository_test

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestLeaseRefreshVersusReleaseSurvivesDeadlock (#256): on MariaDB the
// refresh's locking read walks idx_monitors_lease and then the primary key,
// while the release UPDATE walks the primary key and then rewrites the
// idx_monitors_lease entries it moves. The opposite lock order is an InnoDB
// deadlock (1213) that rolls back one of them. Both are whole-transaction,
// side-effect-free and idempotent, so a concurrent pair must succeed rather
// than surface the deadlock to the scheduler.
func TestLeaseRefreshVersusReleaseSurvivesDeadlock(t *testing.T) {
	const rounds = 150
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			f.monitor(t)
			f.monitor(t)
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "other-worker", 1, time.Minute); err != nil {
				t.Fatal(err)
			}
			failures := map[string]int{}
			for round := 0; round < rounds; round++ {
				if _, err := repo.ClaimBatch(ctx, "lease-worker", 10, time.Minute); err != nil {
					t.Fatalf("round %d claim: %v", round, err)
				}
				var wg sync.WaitGroup
				var refreshErr, releaseErr error
				start := make(chan struct{})
				wg.Add(2)
				go func() { defer wg.Done(); <-start; _, refreshErr = repo.RefreshLease(ctx, "lease-worker", time.Minute) }()
				go func() { defer wg.Done(); <-start; _, releaseErr = repo.ReleaseLeases(ctx, "lease-worker") }()
				close(start)
				wg.Wait()
				if refreshErr != nil {
					failures["refresh"]++
					t.Logf("round %d refresh: %v", round, refreshErr)
				}
				if releaseErr != nil {
					failures["release"]++
					t.Logf("round %d release: %v", round, releaseErr)
				}
			}
			if len(failures) != 0 {
				t.Fatalf("concurrent refresh/release failed in %d rounds: %v", rounds, failures)
			}
		})
	}
}
