package scheduler

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Issue #161: a push monitor whose pushes stop must go DOWN once no push has
// arrived within its interval (Uptime Kuma push semantics), and the next push
// must recover it to UP. Before the fix the scheduler always recorded the
// passive checker's PENDING, so a stopped pusher never alerted.
func TestScheduler_PushMonitorWithoutPushGoesDownAndRecoversOnPush(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 7, Name: "cron", Type: "push", Active: true, Interval: 60,
		PushToken: "tok", Config: map[string]any{"push_token": "tok"},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	sched := NewLocalScheduler(newMockMonitorRepo(mon), repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	check := scheduledCheck{Monitor: mon, CheckConfig: checkConfigForMonitor(mon)}

	// A push arrived (through the push handler path) and is still fresh.
	if err := hbSvc.Record(ctx, mon, ports.CheckResult{Status: domain.StatusUp, Message: "push received"}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	sched.runCheck(ctx, check)
	latest, _ := repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusUp {
		t.Fatalf("fresh push: latest = %v (%q); want UP and no scheduler heartbeat", latest.Status, latest.Msg)
	}
	if n := repo.count(); n != 1 {
		t.Fatalf("fresh push: %d heartbeats; the scheduler must not add one while the push is fresh", n)
	}

	// The pusher stops: the last push is older than the interval.
	repo.mu.Lock()
	latest.Time = time.Now().UTC().Add(-61 * time.Second)
	repo.mu.Unlock()
	sched.runCheck(ctx, check)
	latest, _ = repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusDown || !latest.Important {
		t.Fatalf("missed push: latest = %v important=%v (%q); want an important DOWN", latest.Status, latest.Important, latest.Msg)
	}

	// Still no push on the next tick: stays DOWN (no flip back to PENDING/UP).
	sched.runCheck(ctx, check)
	latest, _ = repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusDown || latest.Important {
		t.Fatalf("still missing: latest = %v important=%v; want a non-important DOWN", latest.Status, latest.Important)
	}

	// The next push recovers the monitor.
	if err := hbSvc.Record(ctx, mon, ports.CheckResult{Status: domain.StatusUp, Message: "push received"}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	latest, _ = repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusUp || !latest.Important {
		t.Fatalf("recovery: latest = %v important=%v; want an important UP", latest.Status, latest.Important)
	}
	before := repo.count()
	sched.runCheck(ctx, check)
	if repo.count() != before {
		t.Fatal("after recovery the fresh push must not get a scheduler heartbeat")
	}
}

// With retries configured, a missed push walks through PENDING to DOWN like
// any other failing check.
func TestScheduler_PushMonitorMissedPushHonoursRetries(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 8, Type: "push", Active: true, Interval: 30, MaxRetries: 1,
		PushToken: "tok8", Config: map[string]any{"push_token": "tok8"},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	sched := NewLocalScheduler(newMockMonitorRepo(mon), repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	check := scheduledCheck{Monitor: mon, CheckConfig: checkConfigForMonitor(mon)}

	// No push ever arrived and the monitor is older than one interval.
	sched.runCheck(ctx, check)
	latest, _ := repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusPending {
		t.Fatalf("first miss with 1 retry: %v; want PENDING", latest.Status)
	}
	sched.runCheck(ctx, check)
	latest, _ = repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusDown || !latest.Important {
		t.Fatalf("second miss: %v important=%v; want important DOWN", latest.Status, latest.Important)
	}
}

// A brand-new push monitor gets one interval before it can go DOWN.
func TestScheduler_NewPushMonitorWaitsOneInterval(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 9, Type: "push", Active: true, Interval: 60,
		PushToken: "tok9", Config: map[string]any{"push_token": "tok9"},
		CreatedAt: time.Now().UTC().Add(-10 * time.Second),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	sched := NewLocalScheduler(newMockMonitorRepo(mon), repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	sched.runCheck(ctx, scheduledCheck{Monitor: mon, CheckConfig: checkConfigForMonitor(mon)})
	if n := repo.count(); n != 0 {
		latest, _ := repo.GetLatest(ctx, mon.ID)
		t.Fatalf("new monitor inside its first interval recorded %d heartbeat(s), latest %v %q; want none", n, latest.Status, latest.Msg)
	}
}
