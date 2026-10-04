package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// orderLog records fleet-relevant scheduler actions in the order they happen, so
// a test can assert attestation precedes lease acquisition rather than merely
// asserting both occurred. Mutex-guarded because Run executes on its own
// goroutine.
type orderLog struct {
	mu     sync.Mutex
	events []string
}

func (l *orderLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// recordingLeaseRepo records lease traffic and satisfies ports.WorkerMonitorReader
// so the sharded scheduler treats it as a real lease reader.
type recordingLeaseRepo struct {
	*mockMonitorRepo
	log *orderLog
}

func (r *recordingLeaseRepo) ClaimBatch(ctx context.Context, worker string, n int, ttl time.Duration) ([]*domain.Monitor, error) {
	r.log.add("claim")
	return r.mockMonitorRepo.ClaimBatch(ctx, worker, n, ttl)
}

func (r *recordingLeaseRepo) RefreshLease(context.Context, string) (int64, error) {
	r.log.add("refresh")
	return 0, nil
}

func (r *recordingLeaseRepo) ListByWorker(context.Context, string, time.Time) ([]*domain.Monitor, error) {
	return nil, nil
}

// recordingReadiness records attestation traffic. Its fields are read by the test
// goroutine only after the scheduler call has returned (or after Run's context is
// canceled and its goroutine joined), so the ordering log is the only state that
// needs a mutex.
type recordingReadiness struct {
	log      *orderLog
	worker   string
	protocol int
	ttl      time.Duration
	err      error
}

func (r *recordingReadiness) DeclareWorker(_ context.Context, workerID string, protocol int, ttl time.Duration) error {
	r.log.add("declare")
	r.worker, r.protocol, r.ttl = workerID, protocol, ttl
	return r.err
}

func (r *recordingReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	return nil, nil
}

// TestShardedSchedulerAttestsBeforeClaiming is the worker half of verification
// matrix T34. Ordering is the property that matters: a worker that leased
// monitors before attesting would appear in the fleet roster as an unattested
// executor and block remote activation for its own fleet. The attestation must
// also carry this worker's identity, the protocol it enforces, and a TTL matching
// its lease TTL so the two expire on the same clock.
func TestShardedSchedulerAttestsBeforeClaiming(t *testing.T) {
	log := &orderLog{}
	readiness := &recordingReadiness{log: log}
	repo := &recordingLeaseRepo{mockMonitorRepo: newMockMonitorRepo(), log: log}
	sched := NewShardedScheduler(repo, func(string) (ports.Checker, bool) { return nil, false }, nil, nil,
		slog.New(slog.DiscardHandler), ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: 2 * time.Minute})
	sched.SetFleetReadiness(readiness)

	sched.refreshAndClaim(context.Background())

	events := log.snapshot()
	if len(events) != 3 || events[0] != "declare" || events[1] != "refresh" || events[2] != "claim" {
		t.Fatalf("event order = %v, want [declare refresh claim]", events)
	}
	if readiness.worker != "worker-a" {
		t.Fatalf("attested worker = %q, want worker-a", readiness.worker)
	}
	if readiness.protocol != ports.HubWorkerAssignmentProtocol {
		t.Fatalf("attested protocol = %d, want %d", readiness.protocol, ports.HubWorkerAssignmentProtocol)
	}
	if readiness.ttl != 2*time.Minute {
		t.Fatalf("attestation ttl = %v, want the lease ttl 2m", readiness.ttl)
	}
}

// TestShardedSchedulerRunAttestsBeforeInitialClaim proves the same ordering on the
// production entry point, not just on the helper the loop calls. Run's very first
// fleet-visible action must be the attestation.
func TestShardedSchedulerRunAttestsBeforeInitialClaim(t *testing.T) {
	log := &orderLog{}
	readiness := &recordingReadiness{log: log}
	repo := &recordingLeaseRepo{mockMonitorRepo: newMockMonitorRepo(), log: log}
	sched := NewShardedScheduler(repo, func(string) (ports.Checker, bool) { return nil, false }, nil, nil,
		slog.New(slog.DiscardHandler), ShardedSchedulerConfig{
			WorkerID: "worker-a", LeaseTTL: time.Minute, PollEvery: time.Hour,
		})
	sched.SetFleetReadiness(readiness)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sched.Run(ctx) }()

	// Wait until the initial claim has happened; declare is synchronous and
	// precedes it, so once "claim" is present the ordering is already determined.
	deadline := time.Now().Add(5 * time.Second)
	for {
		events := log.snapshot()
		if len(events) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("scheduler did not attest and claim within 5s: %v", events)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	events := log.snapshot()
	if events[0] != "declare" || events[1] != "claim" {
		t.Fatalf("Run event order = %v, want declare before claim", events)
	}
}

// TestShardedSchedulerAttestationFailureIsNotFatal proves a readiness outage
// degrades safely: the worker keeps scheduling (availability) while remaining
// unattested, which leaves remote activation refused. Refusing to check monitors
// because an attestation write failed would be the worse failure.
func TestShardedSchedulerAttestationFailureIsNotFatal(t *testing.T) {
	log := &orderLog{}
	readiness := &recordingReadiness{log: log, err: domain.ErrInternal}
	repo := &recordingLeaseRepo{mockMonitorRepo: newMockMonitorRepo(), log: log}
	sched := NewShardedScheduler(repo, func(string) (ports.Checker, bool) { return nil, false }, nil, nil,
		slog.New(slog.DiscardHandler), ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: time.Minute})
	sched.SetFleetReadiness(readiness)

	sched.refreshAndClaim(context.Background())

	events := log.snapshot()
	if len(events) != 3 || events[0] != "declare" || events[2] != "claim" {
		t.Fatalf("a failed attestation must not stop scheduling: %v", events)
	}
}

// TestShardedSchedulerWithoutReadinessStillRuns pins the compatibility boundary:
// a hub with no readiness store attached (probes disabled, or an older
// composition) schedules exactly as before. Attestation is additive.
func TestShardedSchedulerWithoutReadinessStillRuns(t *testing.T) {
	log := &orderLog{}
	repo := &recordingLeaseRepo{mockMonitorRepo: newMockMonitorRepo(), log: log}
	sched := NewShardedScheduler(repo, func(string) (ports.Checker, bool) { return nil, false }, nil, nil,
		slog.New(slog.DiscardHandler), ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: time.Minute})

	sched.refreshAndClaim(context.Background())

	events := log.snapshot()
	if len(events) != 2 || events[0] != "refresh" || events[1] != "claim" {
		t.Fatalf("events without a readiness store = %v, want [refresh claim]", events)
	}
}
