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

type leasedMonitorRepo struct {
	*mockMonitorRepo
	owned  []*domain.Monitor
	err    error
	worker string
	cutoff time.Time
}

func (r *leasedMonitorRepo) ListByWorker(_ context.Context, worker string, cutoff time.Time) ([]*domain.Monitor, error) {
	r.worker, r.cutoff = worker, cutoff
	return r.owned, r.err
}

func TestShardedScheduler_TickUsesCurrentWorkerLeases(t *testing.T) {
	owned := &domain.Monitor{ID: 1, Type: "http", Active: true, Interval: 60}
	other := &domain.Monitor{ID: 2, Type: "http", Active: true, Interval: 60}
	remote := &domain.Monitor{ID: 3, Type: "http", Active: true, Interval: 60}
	for _, tc := range []struct {
		name  string
		owned []*domain.Monitor
		err   error
	}{
		{"owned", []*domain.Monitor{owned}, nil},
		{"remote assignment despite lease", []*domain.Monitor{owned, remote}, nil},
		{"no leases", nil, nil},
		{"database unavailable", nil, errors.New("read leases failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &leasedMonitorRepo{mockMonitorRepo: newMockMonitorRepo(owned, other), owned: tc.owned, err: tc.err}
			sched := NewShardedScheduler(repo, func(string) (ports.Checker, bool) { return nil, false }, nil, nil,
				slog.New(slog.DiscardHandler), ShardedSchedulerConfig{WorkerID: "worker-a", LeaseTTL: time.Minute})
			sched.SetAssignmentRepo(&mockAssignments{remoteOnly: map[int64]struct{}{remote.ID: {}}})
			before := time.Now().UTC().Add(-time.Minute)
			sched.tick(context.Background())
			if _, scheduled := sched.lastCheck.Load(other.ID); scheduled {
				t.Fatal("scheduled a monitor not leased by this worker")
			}
			if _, scheduled := sched.lastCheck.Load(remote.ID); scheduled {
				t.Fatal("scheduled a remote-only assignment")
			}
			if _, scheduled := sched.lastCheck.Load(owned.ID); scheduled != (len(tc.owned) > 0) {
				t.Fatalf("owned monitor scheduled=%v, leases=%d", scheduled, len(tc.owned))
			}
			if repo.worker != "worker-a" || repo.cutoff.Location() != time.UTC || repo.cutoff.Before(before) || repo.cutoff.After(time.Now().UTC().Add(-time.Minute)) {
				t.Fatalf("incorrect lease scope: worker=%q cutoff=%v", repo.worker, repo.cutoff)
			}
		})
	}
}

func TestShardedScheduler_RunRejectsMissingLeaseScope(t *testing.T) {
	for _, tc := range []struct {
		repo     ports.MonitorRepository
		workerID string
	}{
		{newMockMonitorRepo(), "worker-a"},
		{&leasedMonitorRepo{mockMonitorRepo: newMockMonitorRepo()}, ""},
	} {
		sched := NewShardedScheduler(tc.repo, nil, nil, nil, slog.New(slog.DiscardHandler), ShardedSchedulerConfig{WorkerID: tc.workerID})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := sched.Run(ctx); err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("expected missing scope error, got %v", err)
		}
	}
}

func TestShardedScheduler_Run_RecordsAfterCheckContextDeadline(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       2,
			Name:     "slow-http-sharded",
			Type:     "http",
			Active:   true,
			Interval: 1,
			Timeout:  1,
			Config:   map[string]any{"url": "https://example.com"},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	slowChecker := &slowDeadlineChecker{}

	checkerFn := func(t string) (ports.Checker, bool) {
		if t == "http" {
			return slowChecker, true
		}
		return nil, false
	}

	sched := NewShardedScheduler(
		&leasedMonitorRepo{mockMonitorRepo: monitorRepo, owned: monitorRepo.monitors},
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
		ShardedSchedulerConfig{WorkerID: "test-worker"},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	_ = sched.Run(ctx)

	if heartbeatRepo.count() == 0 {
		t.Fatal("expected heartbeat recorded even when check context deadline exceeded")
	}
}

func TestShardedScheduler_CapturesAppliedRevisionAndGeneration(t *testing.T) {
	monitor := &domain.Monitor{
		ID:       1,
		Name:     "sharded",
		Type:     "http",
		Active:   true,
		Interval: 1,
		Timeout:  5,
		Config:   map[string]any{"url": "https://example.com"},
	}
	monitorRepo := newMockMonitorRepo(monitor)
	heartbeatRepo := newMockHeartbeatRepo()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, newMockBus())
	checker := &recordingChecker{}
	sched := NewShardedScheduler(
		&leasedMonitorRepo{mockMonitorRepo: monitorRepo, owned: []*domain.Monitor{monitor}},
		func(string) (ports.Checker, bool) { return checker, true },
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
		ShardedSchedulerConfig{WorkerID: "worker-1"},
	)

	assignments := &mockAssignments{remoteOnly: map[int64]struct{}{}}
	sched.SetAssignmentRepo(assignments)
	sched.SetActivationRepo(&mockActivationRepo{revision: 9})

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = sched.Run(ctx)

	if heartbeatRepo.count() == 0 {
		t.Fatal("expected at least 1 heartbeat")
	}
	latest, err := heartbeatRepo.GetLatest(context.Background(), 1)
	if err != nil || latest == nil {
		t.Fatalf("missing heartbeat for monitor 1: %v", err)
	}
	if latest.ConfigRevision != 9 {
		t.Fatalf("expected ConfigRevision 9, got %d", latest.ConfigRevision)
	}
	if latest.AssignmentGeneration != 1 {
		t.Fatalf("expected AssignmentGeneration 1, got %d", latest.AssignmentGeneration)
	}
}
