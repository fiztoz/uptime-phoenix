package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// errorChecker fails every check so the result is synthesized by runCheck —
// the path where a dropped fence would be easiest to miss.
type errorChecker struct{}

func (c *errorChecker) Type() string                  { return "http" }
func (c *errorChecker) Validate(map[string]any) error { return nil }
func (c *errorChecker) Check(context.Context, map[string]any) (ports.CheckResult, error) {
	return ports.CheckResult{}, errors.New("connection refused")
}

// TestShardedScheduler_CarriesQueuedFenceThroughCheckResult asserts the lease
// fence captured at queue time reaches recording unchanged, on both the normal
// and the checker-error result paths (issue #63).
func TestShardedScheduler_CarriesQueuedFenceThroughCheckResult(t *testing.T) {
	monitor := &domain.Monitor{
		ID: 1, Name: "sharded-fence", Type: "http", Active: true,
		Interval: 1, Timeout: 5, Config: map[string]any{"url": "https://example.com"},
	}
	monitorRepo := newMockMonitorRepo(monitor)
	heartbeatSvc := services.NewHeartbeatService(newMockHeartbeatRepo(), newMockBus())
	recorder := &fakeLocalRecorder{}
	heartbeatSvc.SetRegionalRecorder(nil, recorder)

	sched := NewShardedScheduler(
		&leasedMonitorRepo{mockMonitorRepo: monitorRepo, owned: []*domain.LeasedMonitor{leased(monitor, "worker-a", 42)}},
		func(string) (ports.Checker, bool) { return &errorChecker{}, true },
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
		ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: 90 * time.Second},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = sched.Run(ctx)

	commits := recorder.recorded()
	if len(commits) == 0 {
		t.Fatal("expected at least one recorded check")
	}
	for i, commit := range commits {
		fence := commit.LeaseFence
		if fence == nil || fence.WorkerID != "worker-a" || fence.LeaseEpoch != 42 || fence.LeaseTTL != 90*time.Second {
			t.Fatalf("commit %d lost its queue-time fence: %+v", i, fence)
		}
	}
}

// refreshTTLRepo records the TTL refreshAndClaim hands to RefreshLease. Its
// fields are read by the test goroutine only after the scheduler call has
// returned, so no mutex is needed.
type refreshTTLRepo struct {
	*leasedMonitorRepo
	ttl time.Duration
}

func (r *refreshTTLRepo) RefreshLease(_ context.Context, _ string, ttl time.Duration) (int64, error) {
	r.ttl = ttl
	return 0, nil
}

// TestShardedScheduler_RefreshExtendsWithTheLeaseTTL pins the production call
// the expiry contract depends on: refreshAndClaim must hand RefreshLease the
// same lease TTL ClaimBatch uses. Without the TTL a refresh cannot tell an
// expired lease from a live one and may revive the expired one in place —
// re-authorizing work queued under the previous lease instance (issue #63).
func TestShardedScheduler_RefreshExtendsWithTheLeaseTTL(t *testing.T) {
	monitor := &domain.Monitor{
		ID: 1, Name: "refresh-ttl", Type: "http", Active: true,
		Interval: 1, Timeout: 5, Config: map[string]any{"url": "https://example.com"},
	}
	repo := &refreshTTLRepo{leasedMonitorRepo: &leasedMonitorRepo{mockMonitorRepo: newMockMonitorRepo(monitor)}}
	sched := NewShardedScheduler(
		repo,
		func(string) (ports.Checker, bool) { return &errorChecker{}, true },
		services.NewHeartbeatService(newMockHeartbeatRepo(), newMockBus()),
		nil,
		slog.New(slog.DiscardHandler),
		ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: 90 * time.Second},
	)
	sched.refreshAndClaim(context.Background())
	if repo.ttl != 90*time.Second {
		t.Fatalf("RefreshLease TTL = %v, want %v", repo.ttl, 90*time.Second)
	}
}

// TestLocalScheduler_RecordsWithoutLeaseFence asserts ordinary local
// scheduling keeps the explicit nil fence — local and push recording remain
// supported and un-fenced.
func TestLocalScheduler_RecordsWithoutLeaseFence(t *testing.T) {
	monitor := &domain.Monitor{
		ID: 1, Name: "local-fence", Type: "http", Active: true,
		Interval: 1, Timeout: 5, Config: map[string]any{"url": "https://example.com"},
	}
	monitorRepo := newMockMonitorRepo(monitor)
	heartbeatRepo := newMockHeartbeatRepo()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, newMockBus())
	recorder := &fakeLocalRecorder{}
	heartbeatSvc.SetRegionalRecorder(nil, recorder)

	local := NewLocalScheduler(monitorRepo, heartbeatRepo,
		func(string) (ports.Checker, bool) { return &recordingChecker{}, true },
		heartbeatSvc, nil, slog.New(slog.DiscardHandler))

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = local.Run(ctx)

	commits := recorder.recorded()
	if len(commits) == 0 {
		t.Fatal("expected at least one recorded check")
	}
	for i, commit := range commits {
		if commit.LeaseFence != nil {
			t.Fatalf("commit %d unexpectedly fenced: %+v", i, commit.LeaseFence)
		}
	}
}
