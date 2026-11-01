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
	latest.Time = time.Now().UTC().Add(-63 * time.Second)
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

// setMonitorActive pauses or resumes a monitor the way the database does:
// later reads see a new struct, while checks already in flight keep theirs.
func setMonitorActive(r *mockMonitorRepo, id int64, active bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, m := range r.monitors {
		if m.ID == id {
			c := *m
			c.Active = active
			r.monitors[i] = &c
		}
	}
}

// setLatestTime moves a monitor's latest heartbeat to t the way a database
// write does: later reads see a new struct, while checks already in flight
// keep the one they read.
func setLatestTime(r *mockHeartbeatRepo, monitorID int64, t time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.latestByMID[monitorID]; ok {
		c := *h
		c.Time = t
		r.latestByMID[monitorID] = &c
	}
}

// tickAndSettle runs one scheduler tick and waits for the checks it started.
func tickAndSettle(ctx context.Context, sched *LocalScheduler) {
	sched.tick(ctx)
	time.Sleep(300 * time.Millisecond)
}

// Review of #166: the first tick after a process start (empty in-memory
// state) must not turn the downtime into a DOWN for every push monitor. Each
// push monitor gets one interval (plus grace) from when this scheduler armed
// it, and only then goes DOWN if still no push arrived.
func TestScheduler_PushMonitorFirstTickAfterStartGetsOneInterval(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 10, Type: "push", Active: true, Interval: 60,
		PushToken: "tok10", Config: map[string]any{"push_token": "tok10"},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	if err := hbSvc.Record(ctx, mon, ports.CheckResult{Status: domain.StatusUp, Message: "push received"}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	setLatestTime(repo, mon.ID, time.Now().UTC().Add(-90*time.Second)) // pushed before the restart

	sched := NewLocalScheduler(newMockMonitorRepo(mon), repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	tickAndSettle(ctx, sched)
	if n := repo.count(); n != 1 {
		latest, _ := repo.GetLatest(ctx, mon.ID)
		t.Fatalf("first tick after start recorded %d heartbeat(s), latest %v %q; want none", n-1, latest.Status, latest.Msg)
	}

	// One interval plus grace after arming, still no push: DOWN.
	sched.pushArming.armed.Store(mon.ID, time.Now().UTC().Add(-63*time.Second))
	sched.lastCheck.Delete(mon.ID)
	tickAndSettle(ctx, sched)
	latest, _ := repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusDown || !latest.Important {
		t.Fatalf("after the armed interval: latest = %v important=%v (%q); want an important DOWN", latest.Status, latest.Important, latest.Msg)
	}
}

// A paused monitor is disarmed; when it is resumed it gets a fresh interval
// instead of going DOWN for the pause (pushes are rejected while paused).
func TestScheduler_PushMonitorResumeRearms(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 11, Type: "push", Active: true, Interval: 60,
		PushToken: "tok11", Config: map[string]any{"push_token": "tok11"},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	monitors := newMockMonitorRepo(mon)
	sched := NewLocalScheduler(monitors, repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	if err := hbSvc.Record(ctx, mon, ports.CheckResult{Status: domain.StatusUp, Message: "push received"}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	tickAndSettle(ctx, sched) // armed now, push fresh
	sched.pushArming.armed.Store(mon.ID, time.Now().UTC().Add(-time.Hour))

	setMonitorActive(monitors, mon.ID, false)
	tickAndSettle(ctx, sched)
	if _, armed := sched.pushArming.armed.Load(mon.ID); armed {
		t.Fatal("a paused monitor must be disarmed")
	}

	// Resumed long after its last push.
	setLatestTime(repo, mon.ID, time.Now().UTC().Add(-time.Hour))
	setMonitorActive(monitors, mon.ID, true)
	sched.lastCheck.Delete(mon.ID)
	tickAndSettle(ctx, sched)
	if n := repo.count(); n != 1 {
		latest, _ := repo.GetLatest(ctx, mon.ID)
		t.Fatalf("resume recorded %d heartbeat(s), latest %v %q; want none inside the fresh interval", n-1, latest.Status, latest.Msg)
	}
}

// Review of #166: heartbeats.time is second precision on MariaDB, so a push
// that is not late can look up to a second older than it is, plus scheduler
// jitter. A push stored 60.5 s ago against a 60 s interval is not overdue;
// one stored more than interval+2 s ago is.
func TestScheduler_PushMonitorDeadlineHasGraceForTruncatedTime(t *testing.T) {
	ctx := context.Background()
	mon := &domain.Monitor{
		ID: 12, Type: "push", Active: true, Interval: 60,
		PushToken: "tok12", Config: map[string]any{"push_token": "tok12"},
		CreatedAt: time.Now().UTC().Add(-time.Hour),
	}
	repo := newMockHeartbeatRepo()
	hbSvc := services.NewHeartbeatService(repo, newMockBus())
	sched := NewLocalScheduler(newMockMonitorRepo(mon), repo, checker.Get, hbSvc, nil, slog.New(slog.DiscardHandler))
	check := scheduledCheck{Monitor: mon, CheckConfig: checkConfigForMonitor(mon)}
	if err := hbSvc.Record(ctx, mon, ports.CheckResult{Status: domain.StatusUp, Message: "push received"}); err != nil {
		t.Fatalf("record push: %v", err)
	}
	latest, _ := repo.GetLatest(ctx, mon.ID)

	// Pushed 59.6 s ago at hh:mm:ss.8, stored truncated to the second.
	repo.mu.Lock()
	latest.Time = time.Now().UTC().Add(-59600 * time.Millisecond).Truncate(time.Second)
	repo.mu.Unlock()
	sched.runCheck(ctx, check)
	if n := repo.count(); n != 1 {
		t.Fatalf("on-time push stored truncated recorded %d scheduler heartbeat(s); want none", n-1)
	}

	repo.mu.Lock()
	latest.Time = time.Now().UTC().Add(-62500 * time.Millisecond)
	repo.mu.Unlock()
	sched.runCheck(ctx, check)
	latest, _ = repo.GetLatest(ctx, mon.ID)
	if latest.Status != domain.StatusDown {
		t.Fatalf("push older than interval+grace: latest = %v; want DOWN", latest.Status)
	}
}
