package scheduler

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// The production scheduler path (tick -> captureScheduledChecks -> runCheck
// -> real HTTP checker) decides each check's outbound reach from its
// monitor's owner through AccessService. This package has no TestMain that
// allowlists loopback, so the loopback test server is an ordinary private
// destination here: the admin-owned monitor reaches it, the non-admin-owned
// and the ownerless (deleted-owner) monitors do not, and the server proves it
// by its hit count.
func TestLocalScheduler_OwnerDecidesOutboundReach(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("internal"))
	}))
	defer srv.Close()

	ctx := context.Background()
	users := memory.NewUserRepo()
	admin := &domain.User{Username: "admin", IsAdmin: true}
	member := &domain.User{Username: "member", CanCreateMonitors: true}
	for _, u := range []*domain.User{admin, member} {
		if err := users.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	access := services.NewAccessService(users, memory.NewUserPermissionRepo(), nil, nil)

	monitor := func(id, owner int64) *domain.Monitor {
		return &domain.Monitor{ID: id, UserID: owner, Name: "m", Type: "http", Active: true, Interval: 60, Timeout: 5,
			Config: map[string]any{"url": srv.URL}}
	}
	monitors := newMockMonitorRepo(monitor(1, admin.ID), monitor(2, member.ID), monitor(3, 0))
	heartbeats := newMockHeartbeatRepo()
	checkerFn := func(typ string) (ports.Checker, bool) {
		if typ == "http" {
			return checker.HTTPChecker{}, true
		}
		return nil, false
	}
	sched := NewLocalScheduler(monitors, heartbeats, checkerFn,
		services.NewHeartbeatService(heartbeats, newMockBus()), nil, slog.New(slog.DiscardHandler))
	sched.SetOutboundTrustResolver(access)

	sched.tick(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for !(heartbeats.hasMonitor(1) && heartbeats.hasMonitor(2) && heartbeats.hasMonitor(3)) {
		if time.Now().After(deadline) {
			t.Fatal("scheduler did not record a heartbeat for every monitor")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if n := hits.Load(); n != 1 {
		t.Fatalf("internal server received %d requests; want exactly 1 (the admin-owned monitor)", n)
	}
	adminHB, _ := heartbeats.GetLatest(ctx, 1)
	if adminHB.Status != domain.StatusUp {
		t.Fatalf("admin-owned monitor: %s (%s); want UP", adminHB.Status, adminHB.Msg)
	}
	for _, id := range []int64{2, 3} {
		hb, _ := heartbeats.GetLatest(ctx, id)
		if hb.Status != domain.StatusDown || !strings.Contains(hb.Msg, "destination address is not allowed") {
			t.Fatalf("monitor %d: %s (%q); want DOWN with the netguard refusal", id, hb.Status, hb.Msg)
		}
	}
}

// Without a resolver wired, checks fail closed to the restricted tier.
func TestLocalScheduler_NoResolverIsRestricted(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	monitors := newMockMonitorRepo(&domain.Monitor{ID: 1, UserID: 1, Name: "m", Type: "http", Active: true,
		Interval: 60, Timeout: 5, Config: map[string]any{"url": srv.URL}})
	heartbeats := newMockHeartbeatRepo()
	checkerFn := func(string) (ports.Checker, bool) { return checker.HTTPChecker{}, true }
	sched := NewLocalScheduler(monitors, heartbeats, checkerFn,
		services.NewHeartbeatService(heartbeats, newMockBus()), nil, slog.New(slog.DiscardHandler))

	sched.tick(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for !heartbeats.hasMonitor(1) {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("internal server received %d requests; want 0", n)
	}
}
