package repository_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// readinessLookback is the lease liveness window these cases assert against. It
// stands in for the fleet's shard lease TTL, which the composition root derives
// from SHARD_LEASE_TTL.
const readinessLookback = time.Minute

// leaseMonitors creates n monitors and hands them to worker through the real
// production claim path, so the gate is proven against leases the sharded
// scheduler actually writes rather than hand-inserted rows. The lease TTL is the
// same lookback the assertions use, which is what makes a freshly claimed lease
// live and an aged one dead.
func leaseMonitors(t *testing.T, f probeRegistryFixture, worker string, n int) {
	t.Helper()
	ctx := context.Background()
	monitors := newEngineMonitorRepo(f)
	owner := f.user(t)
	for range n {
		m := &domain.Monitor{UserID: owner, Name: "leased", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
		if err := monitors.Create(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := monitors.ClaimBatch(ctx, worker, n, readinessLookback)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != n {
		t.Fatalf("claimed %d monitors, want %d", len(claimed), n)
	}
}

// ageLeases pushes every lease held by worker into the past by d, simulating a
// worker that stopped refreshing because it is gone.
func ageLeases(t *testing.T, f probeRegistryFixture, worker string, d time.Duration) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(),
		"UPDATE monitors SET leased_at = ? WHERE worker_id = ?",
		time.Now().UTC().Add(-d), worker)
	if err != nil {
		t.Fatal(err)
	}
}

// expireAttestation forces a worker's attestation lease into the past without
// waiting for a real TTL, which keeps the case fast and deterministic.
func expireAttestation(t *testing.T, f probeRegistryFixture, worker string) {
	t.Helper()
	_, err := f.db.ExecContext(context.Background(),
		"UPDATE hub_worker_capabilities SET lease_until = ? WHERE worker_id = ?",
		time.Now().UTC().Add(-time.Hour), worker)
	if err != nil {
		t.Fatal(err)
	}
}

func attestationRows(t *testing.T, f probeRegistryFixture) int {
	t.Helper()
	var count int
	if err := f.db.NewRaw("SELECT COUNT(*) FROM hub_worker_capabilities").Scan(context.Background(), &count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertUnaware(t *testing.T, store *repository.HubWorkerReadinessStore, want ...string) {
	t.Helper()
	got, err := store.UnawareWorkers(context.Background(), ports.HubWorkerAssignmentProtocol, readinessLookback)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("unaware workers = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unaware workers = %v, want %v", got, want)
		}
	}
}

// TestHubWorkerReadinessUnattestedLeaseHolderBlocks is verification matrix T34
// against a real engine: a worker that holds a live monitor lease but never
// attested assignment ownership is exactly an older binary mid-rollout, and it
// must block remote activation. Attesting clears it, and attesting twice is a
// refresh rather than a second identity.
func TestHubWorkerReadinessUnattestedLeaseHolderBlocks(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			leaseMonitors(t, f, "worker-old", 2)
			assertUnaware(t, store, "worker-old")

			if err := store.DeclareWorker(ctx, "worker-old", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			assertUnaware(t, store)

			// The poll loop re-declares every cycle; that must stay one row.
			if err := store.DeclareWorker(ctx, "worker-old", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			if n := attestationRows(t, f); n != 1 {
				t.Fatalf("re-declare left %d rows, want 1", n)
			}
			assertUnaware(t, store)

			// A second, still-unaware worker is reported alongside the first,
			// ordered, so a rollout with several stragglers is fully visible.
			leaseMonitors(t, f, "worker-older", 1)
			assertUnaware(t, store, "worker-older")
		})
	}
}

// TestHubWorkerReadinessDeadWorkerStopsBlocking proves the gate cannot wedge a
// fleet: once an unaware worker's lease ages out it executes nothing, so it must
// stop refusing activation. Without this, one crashed old pod would block remote
// monitoring forever.
func TestHubWorkerReadinessDeadWorkerStopsBlocking(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			store := repository.NewHubWorkerReadinessStore(f.db)

			leaseMonitors(t, f, "worker-gone", 1)
			assertUnaware(t, store, "worker-gone")

			ageLeases(t, f, "worker-gone", 10*readinessLookback)
			assertUnaware(t, store)
		})
	}
}

// TestHubWorkerReadinessStaleProtocolBlocks covers a worker that attests but
// declares an older assignment protocol: it is alive, self-identified as unaware,
// and could claim work at any moment, so it blocks even while holding no lease.
// Re-declaring at the required protocol is what an upgrade looks like.
func TestHubWorkerReadinessStaleProtocolBlocks(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			if err := store.DeclareWorker(ctx, "worker-v0", ports.HubWorkerAssignmentProtocol-1, readinessLookback); err != nil {
				t.Fatal(err)
			}
			assertUnaware(t, store, "worker-v0")

			if err := store.DeclareWorker(ctx, "worker-v0", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			assertUnaware(t, store)
		})
	}
}

// TestHubWorkerReadinessExpiredAttestationBlocksAgain proves a live lease is not
// enough on its own: the attestation has to be current. A worker that stopped
// refreshing its declaration is no longer vouching for itself.
func TestHubWorkerReadinessExpiredAttestationBlocksAgain(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			leaseMonitors(t, f, "worker-stale", 1)
			if err := store.DeclareWorker(ctx, "worker-stale", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			assertUnaware(t, store)

			expireAttestation(t, f, "worker-stale")
			assertUnaware(t, store, "worker-stale")
		})
	}
}

// TestHubWorkerReadinessPrunesExpired proves the attestation table is bounded
// across restarts and rescaled fleets: declaring prunes rows whose lease already
// expired, so identities do not accumulate without limit.
func TestHubWorkerReadinessPrunesExpired(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			if err := store.DeclareWorker(ctx, "worker-departed", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			expireAttestation(t, f, "worker-departed")
			if n := attestationRows(t, f); n != 1 {
				t.Fatalf("rows before prune = %d, want 1", n)
			}
			if err := store.DeclareWorker(ctx, "worker-current", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			if n := attestationRows(t, f); n != 1 {
				t.Fatalf("expired attestation was not pruned: %d rows, want 1", n)
			}
			assertUnaware(t, store)
		})
	}
}

// TestHubWorkerReadinessRejectsInvalidInput proves the store fails loudly on
// unusable arguments instead of writing a row that would vouch for nothing. A
// zero or negative TTL is the dangerous one: it would attest a worker that is
// immediately not live.
func TestHubWorkerReadinessRejectsInvalidInput(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			if err := store.DeclareWorker(ctx, "", 1, readinessLookback); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("empty worker id: %v", err)
			}
			if err := store.DeclareWorker(ctx, "w", -1, readinessLookback); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("negative protocol: %v", err)
			}
			if err := store.DeclareWorker(ctx, "w", 1, 0); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("zero ttl: %v", err)
			}
			if n := attestationRows(t, f); n != 0 {
				t.Fatalf("rejected declarations wrote %d rows", n)
			}
			if _, err := store.UnawareWorkers(ctx, -1, readinessLookback); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("negative required protocol: %v", err)
			}
			if _, err := store.UnawareWorkers(ctx, 1, 0); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("zero lookback: %v", err)
			}
		})
	}
}

// TestHubWorkerReadinessAttestationIsUtcBound is the rule 6 guard for this table.
// Liveness is a wall-clock comparison: lease_until is read back against
// time.Now().UTC(), and monitors.leased_at is written by ClaimBatch in UTC. If
// DeclareWorker persisted a local-zoned wall clock instead, every bound would be
// shifted by the host's UTC offset — on a UTC+7 hub a dead worker would look live
// for seven hours and the gate would pass on false evidence.
//
// The assertion is therefore relative and cross-column: the gap between the
// attestation bound and the lease the same worker holds must be about one TTL,
// not one TTL plus the host offset. This is discriminating on any non-UTC host
// (the recorded run is UTC+7) and merely non-contradictory on a UTC host, which
// is why the mutation check in the acceptance record is the load-bearing evidence.
func TestHubWorkerReadinessAttestationIsUtcBound(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			leaseMonitors(t, f, "worker-tz", 1)
			assertUnaware(t, store, "worker-tz")
			declaredAt := time.Now().UTC()
			if err := store.DeclareWorker(ctx, "worker-tz", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			assertUnaware(t, store)

			var leaseUntil time.Time
			if err := f.db.NewRaw("SELECT lease_until FROM hub_worker_capabilities WHERE worker_id = ?", "worker-tz").
				Scan(ctx, &leaseUntil); err != nil {
				t.Fatal(err)
			}
			var leasedAt time.Time
			if err := f.db.NewRaw("SELECT leased_at FROM monitors WHERE worker_id = ?", "worker-tz").
				Scan(ctx, &leasedAt); err != nil {
				t.Fatal(err)
			}
			// lease_until should sit one TTL after the lease this worker holds.
			// Allow generous slack for second-precision truncation and test
			// runtime; anything within an hour of the offset is a real failure.
			gap := leaseUntil.Sub(leasedAt)
			if gap < 0 || gap > time.Hour {
				t.Fatalf("attestation bound %v vs lease %v: gap %v is not one TTL (%v); the stored wall clock is zone-shifted",
					leaseUntil, leasedAt, gap, readinessLookback)
			}
			// And the bound must be just ahead of a UTC now, not hours ahead.
			ahead := leaseUntil.Sub(declaredAt)
			if ahead < 0 || ahead > readinessLookback+time.Minute {
				t.Fatalf("lease_until is %v ahead of UTC now, want about %v", ahead, readinessLookback)
			}
		})
	}
}

// TestHubWorkerReadinessMigrationRoundTrips proves migration 074's down script
// really drops the attestation table and the up script recreates it, so a
// rollback is not a silent no-op that leaves the gate's storage behind.
//
// The load-bearing assertion is the middle one: with the table gone, the
// readiness query must ERROR. Returning an empty roster instead would report a
// falsely clean fleet and let remote activation through on a hub whose gate
// storage had been rolled back — the exact "stub that returns success" shape
// AGENTS.md rule 7 forbids.
func TestHubWorkerReadinessMigrationRoundTrips(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			store := repository.NewHubWorkerReadinessStore(f.db)

			if err := store.DeclareWorker(ctx, "worker-a", ports.HubWorkerAssignmentProtocol, readinessLookback); err != nil {
				t.Fatal(err)
			}
			if n := attestationRows(t, f); n != 1 {
				t.Fatalf("attestation rows = %d, want 1", n)
			}

			if err := runNamedMigration(t, f, "074_hub_worker_capabilities", "down"); err != nil {
				t.Fatalf("down migration: %v", err)
			}
			if _, err := store.UnawareWorkers(ctx, ports.HubWorkerAssignmentProtocol, readinessLookback); err == nil {
				t.Fatal("readiness query succeeded with no attestation table; it would report a falsely clean fleet")
			}

			if err := runNamedMigration(t, f, "074_hub_worker_capabilities", "up"); err != nil {
				t.Fatalf("up migration: %v", err)
			}
			if n := attestationRows(t, f); n != 0 {
				t.Fatalf("rows after re-up = %d, want 0; the downgrade must have dropped them", n)
			}
			// The gate works again after the round trip.
			leaseMonitors(t, f, "worker-b", 1)
			assertUnaware(t, store, "worker-b")
		})
	}
}
