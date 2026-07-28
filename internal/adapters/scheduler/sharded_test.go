package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

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
		monitorRepo,
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
