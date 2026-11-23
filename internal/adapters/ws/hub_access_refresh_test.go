package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// gatedListRepo counts the hub's List calls and, when gate is set, blocks each
// List until the gate channel is closed. It stands in for the production
// snapshot read (List + tags + latest statuses), which took seconds per call
// at 1,000 monitors on the M6 F2 hub.
type gatedListRepo struct {
	*hubFakeMonitorRepo
	calls atomic.Int64
	gate  chan struct{}
}

func (r *gatedListRepo) List(ctx context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.calls.Add(1)
	if r.gate != nil {
		select {
		case <-r.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return r.hubFakeMonitorRepo.List(ctx, filter)
}

type accessRefreshHarness struct {
	hub      *Hub
	bus      *eventbus.MemoryBus
	repo     *gatedListRepo
	access   *services.AccessService
	users    *memory.UserRepo
	adminID  int64
	memberID int64
	granted  int64
}

func newAccessRefreshHarness(t *testing.T, gate chan struct{}) *accessRefreshHarness {
	t.Helper()
	ctx := context.Background()
	repo := &gatedListRepo{hubFakeMonitorRepo: newHubFakeMonitorRepo(), gate: gate}
	granted := &domain.Monitor{UserID: 1, Name: "granted", Type: "http", Active: true, Interval: 60}
	if err := repo.Create(ctx, granted); err != nil {
		t.Fatal(err)
	}
	users := memory.NewUserRepo()
	admin := &domain.User{Username: "admin", Active: true, IsAdmin: true}
	member := &domain.User{Username: "member", Active: true}
	for _, u := range []*domain.User{admin, member} {
		if err := users.Create(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	perms := memory.NewUserPermissionRepo()
	if err := perms.Grant(ctx, &domain.UserPermission{UserID: member.ID, MonitorID: &granted.ID}); err != nil {
		t.Fatal(err)
	}
	access := services.NewAccessService(users, perms, nil, repo.hubFakeMonitorRepo)
	bus := eventbus.NewMemoryBus()
	t.Cleanup(bus.Close)
	h := NewHub(bus, repo, newHubFakeHeartbeatRepo(), access, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if !h.waitReady(2 * time.Second) {
		t.Fatal("hub never subscribed to the event bus")
	}
	return &accessRefreshHarness{hub: h, bus: bus, repo: repo, access: access, users: users, adminID: admin.ID, memberID: member.ID, granted: granted.ID}
}

func (a *accessRefreshHarness) connect(t *testing.T, userID int64) *Client {
	t.Helper()
	c := NewClient("access-refresh", userID)
	a.hub.AddClient(c)
	t.Cleanup(func() { a.hub.RemoveClient(c) })
	return c
}

func (a *accessRefreshHarness) invalidate(t *testing.T, userID int64, times int) {
	t.Helper()
	for i := 0; i < times; i++ {
		if err := a.bus.Publish(context.Background(), ports.Event{Type: "access.invalidate", Payload: map[string]any{"user_id": userID}}); err != nil {
			t.Fatal(err)
		}
	}
}

func (a *accessRefreshHarness) heartbeat(t *testing.T) {
	t.Helper()
	if err := a.bus.Publish(context.Background(), ports.Event{Type: EventHeartbeat, Payload: &domain.Heartbeat{
		MonitorID: a.granted, Status: domain.StatusUp, Time: time.Now().UTC(),
	}}); err != nil {
		t.Fatal(err)
	}
}

// collectFrames reads frame types until quiet has passed with no new frame.
func collectFrames(t *testing.T, c *Client, quiet time.Duration) []string {
	t.Helper()
	var types []string
	for {
		select {
		case raw := <-c.Outbound():
			var frame struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			types = append(types, frame.Type)
		case <-time.After(quiet):
			return types
		}
	}
}

func countType(types []string, want string) int {
	n := 0
	for _, got := range types {
		if got == want {
			n++
		}
	}
	return n
}

// Every monitor an admin creates writes a creator grant, and every grant
// publishes access.invalidate for that admin. An admin sees every monitor
// whatever its grants say, so none of those invalidations changes what the
// dashboard may show. The hub used to answer each one with access.changed (the
// dashboard wipes its state) plus a full monitor.list rebuild, inline on the
// fan-out loop: 1,000 creates kept the M6 F2 hub CPU at 98 % for about ten
// minutes and pushed WebSocket fan-out p95 to 17 s.
func TestAccessInvalidationWithUnchangedAllScopeSendsNoSnapshot(t *testing.T) {
	a := newAccessRefreshHarness(t, nil)
	admin := a.connect(t, a.adminID)
	a.hub.sendMonitorList(context.Background(), admin)
	if got := collectFrames(t, admin, 100*time.Millisecond); countType(got, EventMonitorList) != 1 {
		t.Fatalf("connect snapshot frames = %v", got)
	}
	listsBefore := a.repo.calls.Load()

	a.invalidate(t, a.adminID, 50)
	a.heartbeat(t)

	got := collectFrames(t, admin, 500*time.Millisecond)
	if countType(got, EventHeartbeat) != 1 {
		t.Fatalf("heartbeat not delivered after invalidations: %v", got)
	}
	if n := countType(got, "access.changed") + countType(got, EventMonitorList); n != 0 {
		t.Fatalf("admin with unchanged all-monitor scope got %d access.changed/monitor.list frames for 50 invalidations: %v", n, got)
	}
	if extra := a.repo.calls.Load() - listsBefore; extra != 0 {
		t.Fatalf("unchanged scope rebuilt the monitor list %d times", extra)
	}
}

// A slow snapshot rebuild must never hold up heartbeat fan-out: the refresh
// runs off the listen loop.
func TestAccessRefreshDoesNotBlockHeartbeatFanOut(t *testing.T) {
	gate := make(chan struct{})
	a := newAccessRefreshHarness(t, gate)
	member := a.connect(t, a.memberID)

	a.invalidate(t, a.memberID, 1)
	deadline := time.Now().Add(2 * time.Second)
	for a.repo.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.repo.calls.Load() == 0 {
		close(gate)
		t.Fatal("access refresh never started its monitor list read")
	}

	a.heartbeat(t)
	got := collectFrames(t, member, 300*time.Millisecond)
	close(gate)
	if countType(got, EventHeartbeat) != 1 {
		t.Fatalf("heartbeat waited behind a blocked monitor.list rebuild; frames while blocked: %v", got)
	}
	after := collectFrames(t, member, 300*time.Millisecond)
	if countType(after, EventMonitorList) != 1 {
		t.Fatalf("refresh did not finish with one monitor.list after the read was released: %v", after)
	}
}

// A burst of invalidations for one user collapses into at most one refresh in
// flight plus one trailing refresh, and the last snapshot reflects the final
// grants.
func TestAccessInvalidationBurstCoalesces(t *testing.T) {
	a := newAccessRefreshHarness(t, nil)
	member := a.connect(t, a.memberID)

	a.invalidate(t, a.memberID, 100)
	got := collectFrames(t, member, 500*time.Millisecond)
	lists := countType(got, EventMonitorList)
	if lists < 1 || lists > 2 {
		t.Fatalf("100 invalidations produced %d monitor.list frames, want 1 or 2: %v", lists, got)
	}
	if calls := a.repo.calls.Load(); calls > 2 {
		t.Fatalf("100 invalidations rebuilt the monitor list %d times, want at most 2", calls)
	}
	if got[len(got)-1] != EventMonitorList || got[0] != "access.changed" {
		t.Fatalf("refresh must announce access.changed before its snapshot: %v", got)
	}
}

// The skip is only for a client whose scope is still "all monitors". A user
// who loses all-monitor visibility (admin removed, or CanViewAllMonitors
// cleared) must get access.changed and a snapshot narrowed to their grants,
// even though their previous snapshot was all-scope.
func TestAccessInvalidationAfterLosingAllScopeSendsNarrowedSnapshot(t *testing.T) {
	for name, demote := range map[string]struct {
		grant  func(*domain.User)
		revoke func(*domain.User)
	}{
		"admin":              {func(u *domain.User) { u.IsAdmin = true }, func(u *domain.User) { u.IsAdmin = false }},
		"CanViewAllMonitors": {func(u *domain.User) { u.CanViewAllMonitors = true }, func(u *domain.User) { u.CanViewAllMonitors = false }},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			a := newAccessRefreshHarness(t, nil)
			other := &domain.Monitor{UserID: 1, Name: "not granted", Type: "http", Active: true, Interval: 60}
			if err := a.repo.Create(ctx, other); err != nil {
				t.Fatal(err)
			}
			user, err := a.users.GetByID(ctx, a.memberID)
			if err != nil {
				t.Fatal(err)
			}
			demote.grant(user)
			if err := a.users.Update(ctx, user); err != nil {
				t.Fatal(err)
			}
			c := a.connect(t, a.memberID)
			a.hub.sendMonitorList(ctx, c)
			if got := snapshotSizes(t, c, 100*time.Millisecond); len(got) != 1 || got[0] != 2 {
				t.Fatalf("all-scope connect snapshot sizes = %v, want [2]", got)
			}

			demote.revoke(user)
			if err := a.users.Update(ctx, user); err != nil {
				t.Fatal(err)
			}
			a.invalidate(t, a.memberID, 1)

			raw := collectRaw(t, c, 500*time.Millisecond)
			if len(raw) == 0 || raw[0].Type != "access.changed" {
				t.Fatalf("losing all-monitor scope sent no access.changed first: %v", raw)
			}
			last := raw[len(raw)-1]
			if last.Type != EventMonitorList || len(last.Monitors) != 1 || last.Monitors[0].ID != a.granted {
				t.Fatalf("snapshot after losing all-monitor scope = %+v, want only the granted monitor %d", last, a.granted)
			}
		})
	}
}

type rawFrame struct {
	Type     string
	Monitors []struct {
		ID int64 `json:"id"`
	}
}

// collectRaw reads frames (type plus monitor.list members) until quiet passes.
func collectRaw(t *testing.T, c *Client, quiet time.Duration) []rawFrame {
	t.Helper()
	var out []rawFrame
	for {
		select {
		case raw := <-c.Outbound():
			var frame struct {
				Type    string          `json:"type"`
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			f := rawFrame{Type: frame.Type}
			if frame.Type == EventMonitorList {
				if err := json.Unmarshal(frame.Payload, &f.Monitors); err != nil {
					t.Fatal(err)
				}
			}
			out = append(out, f)
		case <-time.After(quiet):
			return out
		}
	}
}

func snapshotSizes(t *testing.T, c *Client, quiet time.Duration) []int {
	t.Helper()
	var sizes []int
	for _, f := range collectRaw(t, c, quiet) {
		if f.Type == EventMonitorList {
			sizes = append(sizes, len(f.Monitors))
		}
	}
	return sizes
}
