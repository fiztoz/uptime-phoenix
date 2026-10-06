package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Lease fencing contract (issue #63): a result captured under an expired or
// replaced worker lease must be rejected atomically — no heartbeat history, no
// regional state, no retry state, no incidents, and no delivery work — while
// current-owner checks and unfenced local/push recording keep working.
//
// Expiry is simulated by backdating leased_at (deterministic wall-clock), not
// by sleeping: the production rule is "the lease is valid while
// leased_at >= now - leaseTTL", and ClaimBatch is the only writer that creates
// a new lease instance.

const fenceTTL = time.Minute

// fenceSideEffectTables are the write surfaces of one local heartbeat commit.
var fenceSideEffectTables = []string{
	"heartbeats", "probe_observations", "monitor_probe_state", "probe_dirty_buckets",
	"alerts", "probe_incidents", "probe_delivery_intents", "notification_throttles",
	"alert_escalations",
}

func fenceSideEffects(t *testing.T, f probeRegistryFixture, monitorID int64) map[string]int {
	t.Helper()
	ctx := context.Background()
	out := make(map[string]int, len(fenceSideEffectTables))
	for _, table := range fenceSideEffectTables {
		var n int
		if err := f.db.NewSelect().Table(table).ColumnExpr("COUNT(*)").Where("monitor_id = ?", monitorID).Scan(ctx, &n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		out[table] = n
	}
	return out
}

func assertNoFenceSideEffects(t *testing.T, f probeRegistryFixture, monitorID int64, before map[string]int) {
	t.Helper()
	after := fenceSideEffects(t, f, monitorID)
	for table, n := range before {
		if after[table] != n {
			t.Fatalf("rejected commit changed %s: %d -> %d", table, n, after[table])
		}
	}
	if state, err := f.commits.GetState(context.Background(), monitorID, domain.LocalProbeID); err == nil || state != nil {
		t.Fatalf("rejected commit left regional state: %+v %v", state, err)
	} else if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("state read failed: %v", err)
	}
	if seq := localSequence(t, f); seq != 0 {
		t.Fatalf("rejected commit consumed sequence %d", seq)
	}
}

// claimFence acquires a lease for worker and returns the fence a queued check
// would capture at schedule time.
func claimFence(t *testing.T, f probeRegistryFixture, monitorID int64, worker string) *domain.LeaseFence {
	t.Helper()
	ctx := context.Background()
	repo := newEngineMonitorRepo(f)
	if _, err := repo.ClaimBatch(ctx, worker, 1000, fenceTTL); err != nil {
		t.Fatalf("claim as %s: %v", worker, err)
	}
	leases, err := repo.(ports.WorkerMonitorReader).ListByWorker(ctx, worker, time.Now().UTC().Add(-fenceTTL))
	if err != nil {
		t.Fatal(err)
	}
	for _, lease := range leases {
		if lease.Monitor.ID == monitorID {
			if lease.WorkerID != worker || lease.LeasedAt.IsZero() {
				t.Fatalf("lease identity dropped: %+v", lease)
			}
			return &domain.LeaseFence{WorkerID: lease.WorkerID, LeaseEpoch: lease.LeaseEpoch, LeaseTTL: fenceTTL}
		}
	}
	t.Fatalf("monitor %d not leased by %s", monitorID, worker)
	return nil
}

// expireLease simulates natural expiry with failed renewals: nothing re-stamps
// leased_at before the validity window closes.
func expireLease(t *testing.T, f probeRegistryFixture, monitorID int64) {
	t.Helper()
	lapsed := time.Now().UTC().Add(-10 * fenceTTL)
	if _, err := f.db.ExecContext(context.Background(), "UPDATE monitors SET leased_at = ? WHERE id = ?", lapsed, monitorID); err != nil {
		t.Fatal(err)
	}
}

func leaseEpochOf(t *testing.T, f probeRegistryFixture, monitorID int64) int64 {
	t.Helper()
	var epoch int64
	if err := f.db.NewSelect().Table("monitors").Column("lease_epoch").Where("id = ?", monitorID).Scan(context.Background(), &epoch); err != nil {
		t.Fatal(err)
	}
	return epoch
}

// fencedDownCommit carries the full side-effect payload (incident, alert,
// escalation, throttle, delivery intent) so a rejection proves those surfaces
// stayed untouched.
func fencedDownCommit(t *testing.T, f probeRegistryFixture, monitorID int64, fence *domain.LeaseFence) domain.LocalHeartbeatCommit {
	t.Helper()
	uID := f.user(t)
	notifID := f.notification(t, uID)
	policyID := f.escalationPolicy(t, uID)
	at := time.Now().UTC()
	return domain.LocalHeartbeatCommit{
		Heartbeat: domain.Heartbeat{
			MonitorID: monitorID, ProbeID: domain.LocalProbeID, StreamID: domain.LocalStreamID,
			AssignmentGeneration: 1, ConfigRevision: 1, Status: domain.StatusDown, DownCount: 1,
			Time: at, ReceivedAt: at, Msg: "Connection refused",
		},
		RawStatus:        domain.StatusDown,
		ExpectedStateSeq: 0,
		Incident:         &domain.RegionalIncident{Status: domain.AlertStatusFiring},
		Alert:            &domain.Alert{Status: domain.AlertStatusFiring},
		Escalation:       &domain.AlertEscalation{PolicyID: policyID, NextStep: 1, NextRunAt: at.Add(5 * time.Minute)},
		ThrottleUpdate:   true,
		DeliveryIntents:  []domain.DeliveryIntent{{NotificationID: notifID, NotificationVersion: 1}},
		LeaseFence:       fence,
	}
}

func TestLocalHeartbeatLeaseFenceContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ExpiredLeaseAfterFailedRenewals", func(t *testing.T) {
				testFenceExpiredLease(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("TakeoverRejectsPriorOwner", func(t *testing.T) {
				testFenceTakeover(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("SameWorkerReacquisitionRejectsOldFence", func(t *testing.T) {
				testFenceReacquisition(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("RefreshCannotReviveExpiredLease", func(t *testing.T) {
				testFenceRefreshNoRevive(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("RenewalKeepsAuthority", func(t *testing.T) {
				testFenceRenewal(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("NilFencePreservesLocalAndPush", func(t *testing.T) {
				testFenceNilPreserved(t, newProbeRegistryFixture(t, engine))
			})
			t.Run("InvalidFenceRejected", func(t *testing.T) {
				testFenceValidation(t, newProbeRegistryFixture(t, engine))
			})
		})
	}
}

func testFenceExpiredLease(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	fence := claimFence(t, f, id, "worker-a")

	// Natural expiry with failed renewals: the lease lapses untouched.
	expireLease(t, f, id)
	before := fenceSideEffects(t, f, id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fence))
	if !errors.Is(err, ports.ErrStaleLease) || hb != nil {
		t.Fatalf("expired lease committed: hb=%+v err=%v", hb, err)
	}
	assertNoFenceSideEffects(t, f, id, before)

	// Authority restored by re-queueing under the current lease commits again.
	fresh := claimFence(t, f, id, "worker-a")
	if fresh.LeaseEpoch == fence.LeaseEpoch {
		t.Fatal("reacquisition after expiry kept the lease instance")
	}
	hb, err = f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fresh))
	if err != nil || hb == nil || hb.SourceSeq != 1 {
		t.Fatalf("current-owner commit failed: %+v %v", hb, err)
	}
}

func testFenceTakeover(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	fenceA := claimFence(t, f, id, "worker-a")
	expireLease(t, f, id)
	fenceB := claimFence(t, f, id, "worker-b")
	if fenceA.WorkerID == fenceB.WorkerID || fenceA.LeaseEpoch == fenceB.LeaseEpoch {
		t.Fatalf("takeover kept the lease instance: %+v -> %+v", fenceA, fenceB)
	}

	before := fenceSideEffects(t, f, id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fenceA))
	if !errors.Is(err, ports.ErrStaleLease) || hb != nil {
		t.Fatalf("replaced worker committed: hb=%+v err=%v", hb, err)
	}
	assertNoFenceSideEffects(t, f, id, before)

	// The replacement owner (worker-b) records normally — its DOWN survives.
	hb, err = f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fenceB))
	if err != nil || hb == nil || hb.SourceSeq != 1 || hb.Status != domain.StatusDown {
		t.Fatalf("replacement owner commit failed: %+v %v", hb, err)
	}
}

func testFenceReacquisition(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	fence1 := claimFence(t, f, id, "worker-a")
	expireLease(t, f, id)
	fence2 := claimFence(t, f, id, "worker-a")
	if fence1.LeaseEpoch == fence2.LeaseEpoch {
		t.Fatalf("same-worker reacquisition kept the lease instance: %d", fence1.LeaseEpoch)
	}

	before := fenceSideEffects(t, f, id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fence1))
	if !errors.Is(err, ports.ErrStaleLease) || hb != nil {
		t.Fatalf("pre-reacquisition work committed: hb=%+v err=%v", hb, err)
	}
	assertNoFenceSideEffects(t, f, id, before)

	hb, err = f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fence2))
	if err != nil || hb == nil || hb.SourceSeq != 1 {
		t.Fatalf("post-reacquisition commit failed: %+v %v", hb, err)
	}
}

// testFenceRefreshNoRevive is the regression for the refresh-before-claim
// revival: EXPIRED -> RefreshLease -> ClaimBatch used to re-stamp the expired
// lease in place (same epoch), so the claim read as a renewal, no new lease
// instance was ever established, and work queued under the pre-expiry instance
// kept its authority. A refresh must leave expired rows alone so the claim
// that follows it is a reacquisition.
func testFenceRefreshNoRevive(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	repo := newEngineMonitorRepo(f)
	fence := claimFence(t, f, id, "worker-a")

	expireLease(t, f, id)
	n, err := repo.RefreshLease(ctx, "worker-a", fenceTTL)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("refresh extended %d expired lease(s)", n)
	}
	if got := leaseEpochOf(t, f, id); got != fence.LeaseEpoch {
		t.Fatalf("refresh moved the lease instance: %d -> %d", fence.LeaseEpoch, got)
	}

	// EXPIRED -> RefreshLease -> ClaimBatch: the claim must still be a
	// reacquisition (new instance), never a renewal of the revived row.
	fresh := claimFence(t, f, id, "worker-a")
	if fresh.LeaseEpoch == fence.LeaseEpoch {
		t.Fatal("claim after refresh kept the pre-expiry lease instance")
	}

	before := fenceSideEffects(t, f, id)
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fence))
	if !errors.Is(err, ports.ErrStaleLease) || hb != nil {
		t.Fatalf("pre-expiry fence committed after refresh: hb=%+v err=%v", hb, err)
	}
	assertNoFenceSideEffects(t, f, id, before)

	hb, err = f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fresh))
	if err != nil || hb == nil || hb.SourceSeq != 1 {
		t.Fatalf("current-owner commit failed: %+v %v", hb, err)
	}
}

func testFenceRenewal(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	repo := newEngineMonitorRepo(f)
	fence := claimFence(t, f, id, "worker-a")
	epoch := leaseEpochOf(t, f, id)

	// MariaDB counts changed rows, not matched rows. Age this still-live
	// lease so second-precision storage cannot make renewal a no-op.
	if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET leased_at = ? WHERE id = ?", time.Now().UTC().Add(-fenceTTL/2), id); err != nil {
		t.Fatal(err)
	}
	if n, err := repo.RefreshLease(ctx, "worker-a", fenceTTL); err != nil || n != 1 {
		t.Fatalf("refresh live lease: n=%d err=%v", n, err)
	}
	// A re-stamp through ClaimBatch's own-worker arm is a renewal too.
	if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, fenceTTL); err != nil {
		t.Fatal(err)
	}
	if got := leaseEpochOf(t, f, id); got != epoch {
		t.Fatalf("renewal changed the lease instance: %d -> %d", epoch, got)
	}

	// Normal renewal keeps in-flight work authorizable.
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, fencedDownCommit(t, f, id, fence))
	if err != nil || hb == nil || hb.SourceSeq != 1 {
		t.Fatalf("renewed lease lost authority: %+v %v", hb, err)
	}
}

func testFenceNilPreserved(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)

	// Ordinary local/push recording carries no worker authority at all.
	hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
	if err != nil || hb == nil || hb.SourceSeq != 1 {
		t.Fatalf("unfenced commit without leases failed: %+v %v", hb, err)
	}
	// Even while another worker owns the lease, a nil fence is deliberately
	// not subject to worker fencing.
	claimFence(t, f, id, "worker-x")
	hb, err = f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 1))
	if err != nil || hb == nil || hb.SourceSeq != 2 {
		t.Fatalf("unfenced commit under an active lease failed: %+v %v", hb, err)
	}
}

func testFenceValidation(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	valid := claimFence(t, f, id, "worker-a")
	for _, tc := range []struct {
		name  string
		fence domain.LeaseFence
	}{
		{"empty worker", domain.LeaseFence{LeaseEpoch: valid.LeaseEpoch, LeaseTTL: fenceTTL}},
		{"non-positive ttl", domain.LeaseFence{WorkerID: "worker-a", LeaseEpoch: valid.LeaseEpoch}},
	} {
		commit := localCommit(id, 0)
		commit.LeaseFence = &tc.fence
		hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, commit)
		if !errors.Is(err, domain.ErrValidation) || hb != nil {
			t.Fatalf("%s fence accepted: %+v %v", tc.name, hb, err)
		}
	}
}

// TestMonitorLeaseEpochMigrationRoundTrip proves 075 down/up round-trips on
// both engines and epoch fencing keeps working afterwards. The MariaDB schema
// is shared with sibling packages, so the column is restored even on failure.
func TestMonitorLeaseEpochMigrationRoundTrip(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := f.monitor(t)
			t.Cleanup(func() {
				// Idempotent on MariaDB (ADD COLUMN IF NOT EXISTS); harmless
				// error on SQLite when the rehearsal already restored it.
				_ = runEngineMigration(t, f.db, engine, "075_monitor_lease_epoch", "up")
			})
			if err := runEngineMigration(t, f.db, engine, "075_monitor_lease_epoch", "down"); err != nil {
				t.Fatalf("075 down: %v", err)
			}
			if leaseEpochColumn(t, f) {
				t.Fatal("075 down left monitors.lease_epoch behind")
			}
			if err := runEngineMigration(t, f.db, engine, "075_monitor_lease_epoch", "up"); err != nil {
				t.Fatalf("075 up: %v", err)
			}
			if !leaseEpochColumn(t, f) {
				t.Fatal("075 up did not restore monitors.lease_epoch")
			}
			repo := newEngineMonitorRepo(f)
			if _, err := repo.ClaimBatch(ctx, "worker-a", 1000, fenceTTL); err != nil {
				t.Fatal(err)
			}
			if got := leaseEpochOf(t, f, id); got != 1 {
				t.Fatalf("post-migration epoch = %d, want 1", got)
			}
		})
	}
}

func leaseEpochColumn(t *testing.T, f probeRegistryFixture) bool {
	t.Helper()
	var count int
	var err error
	if f.engine == "mariadb" {
		err = f.db.NewRaw(`SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'monitors' AND column_name = 'lease_epoch'`).
			Scan(context.Background(), &count)
	} else {
		err = f.db.NewRaw(fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('monitors') WHERE name = %q", "lease_epoch")).
			Scan(context.Background(), &count)
	}
	if err != nil {
		t.Fatal(err)
	}
	return count == 1
}
