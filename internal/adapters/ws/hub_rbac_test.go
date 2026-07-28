package ws

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- Test doubles ---------------------------------------------------------

type hubFakeMonitorRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.Monitor
	nextID int64
}

func newHubFakeMonitorRepo() *hubFakeMonitorRepo {
	return &hubFakeMonitorRepo{byID: make(map[int64]*domain.Monitor)}
}

func (r *hubFakeMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	m.ID = r.nextID
	cp := *m
	r.byID[m.ID] = &cp
	return nil
}

func (r *hubFakeMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *hubFakeMonitorRepo) GetByPushToken(_ context.Context, _ string) (*domain.Monitor, error) {
	return nil, ports.ErrNotFound
}

// List honors RestrictToIDs the way the SQL adapters do — on the FLAG, not on
// len(MonitorIDs). A fake that ignored an empty allowlist would let the hub hand a
// grant-less client the whole install and this test would still pass.
func (r *hubFakeMonitorRepo) List(_ context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var allowed map[int64]bool
	if filter.RestrictToIDs {
		allowed = make(map[int64]bool, len(filter.MonitorIDs))
		for _, id := range filter.MonitorIDs {
			allowed[id] = true
		}
	}
	out := make([]*domain.Monitor, 0, len(r.byID))
	for _, m := range r.byID {
		if filter.RestrictToIDs && !allowed[m.ID] {
			continue
		}
		cp := *m
		out = append(out, &cp)
	}
	return out, nil
}

func (r *hubFakeMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Monitor, 0, len(r.byID))
	for _, m := range r.byID {
		if m.Active {
			cp := *m
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *hubFakeMonitorRepo) Update(_ context.Context, _ *domain.Monitor) error { return nil }
func (r *hubFakeMonitorRepo) Delete(_ context.Context, _ int64) error           { return nil }
func (r *hubFakeMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *hubFakeMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error)  { return 0, nil }
func (r *hubFakeMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) { return 0, nil }

type hubFakeHeartbeatRepo struct {
	mu     sync.Mutex
	latest map[int64]*domain.Heartbeat
}

func newHubFakeHeartbeatRepo() *hubFakeHeartbeatRepo {
	return &hubFakeHeartbeatRepo{latest: make(map[int64]*domain.Heartbeat)}
}

func (r *hubFakeHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[h.MonitorID] = h
	return nil
}

func (r *hubFakeHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	hb, ok := r.latest[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return hb, nil
}

func (r *hubFakeHeartbeatRepo) ListByMonitor(_ context.Context, _ int64, _, _ time.Time) ([]*domain.Heartbeat, error) {
	return nil, nil
}
func (r *hubFakeHeartbeatRepo) DeleteByMonitor(_ context.Context, _ int64) error     { return nil }
func (r *hubFakeHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error { return nil }
func (r *hubFakeHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *hubFakeHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *hubFakeHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *hubFakeHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *hubFakeHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *hubFakeHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// --- Harness --------------------------------------------------------------

type hubRBACHarness struct {
	hub      *Hub
	bus      *eventbus.MemoryBus
	monitors *hubFakeMonitorRepo

	adminID    int64
	memberID   int64 // granted monitorGranted only
	strangerID int64 // no grants at all

	monitorGranted int64
	monitorHidden  int64
}

func newHubRBACHarness(t *testing.T) *hubRBACHarness {
	t.Helper()
	ctx := context.Background()

	monitors := newHubFakeMonitorRepo()
	heartbeats := newHubFakeHeartbeatRepo()

	granted := &domain.Monitor{UserID: 1, Name: "granted", Type: "http", Active: true, Interval: 60}
	hidden := &domain.Monitor{UserID: 1, Name: "hidden", Type: "http", Active: true, Interval: 60}
	if err := monitors.Create(ctx, granted); err != nil {
		t.Fatalf("create granted monitor: %v", err)
	}
	if err := monitors.Create(ctx, hidden); err != nil {
		t.Fatalf("create hidden monitor: %v", err)
	}

	users := memory.NewUserRepo()
	admin := &domain.User{Username: "admin", Active: true, IsAdmin: true}
	member := &domain.User{Username: "member", Active: true}
	stranger := &domain.User{Username: "stranger", Active: true}
	for _, u := range []*domain.User{admin, member, stranger} {
		if err := users.Create(ctx, u); err != nil {
			t.Fatalf("create user %s: %v", u.Username, err)
		}
	}

	perms := memory.NewUserPermissionRepo()
	if err := perms.Grant(ctx, &domain.UserPermission{UserID: member.ID, MonitorID: &granted.ID}); err != nil {
		t.Fatalf("grant: %v", err)
	}

	access := services.NewAccessService(users, perms, nil, monitors)

	bus := eventbus.NewMemoryBus()
	t.Cleanup(bus.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(bus, monitors, heartbeats, access, nil, log)

	// NewHub subscribes inside a goroutine. Publishing before it has registered
	// means the event is dropped and the test proves nothing, so wait for it.
	if !hub.waitReady(2 * time.Second) {
		t.Fatal("hub never subscribed to the event bus")
	}

	return &hubRBACHarness{
		hub:            hub,
		bus:            bus,
		monitors:       monitors,
		adminID:        admin.ID,
		memberID:       member.ID,
		strangerID:     stranger.ID,
		monitorGranted: granted.ID,
		monitorHidden:  hidden.ID,
	}
}

// addClient registers a client on the hub. It does NOT call sendMonitorList, so a
// test only ever sees the frames its own Publish produced.
func (h *hubRBACHarness) addClient(userID int64) *Client {
	c := NewClient("test-client", userID)
	h.hub.AddClient(c)
	return c
}

// awaitFrames collects every frame the client receives within a quiet window.
// It returns after the client has been silent for `quiet`, so it detects both
// "the frame arrived" and "no frame ever arrived" without a fixed long sleep.
func awaitFrames(c *Client, quiet time.Duration) []map[string]any {
	var out []map[string]any
	for {
		select {
		case data := <-c.Outbound():
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err == nil {
				out = append(out, frame)
			}
		case <-time.After(quiet):
			return out
		}
	}
}

// heartbeatFramesFor returns the heartbeat frames in `frames` for one monitor.
func heartbeatFramesFor(frames []map[string]any, monitorID int64) []map[string]any {
	var out []map[string]any
	for _, f := range frames {
		if f["type"] != EventHeartbeat {
			continue
		}
		payload, ok := f["payload"].(map[string]any)
		if !ok {
			continue
		}
		id, ok := payload["monitor_id"].(float64)
		if !ok {
			continue
		}
		if int64(id) == monitorID {
			out = append(out, f)
		}
	}
	return out
}

// --- Tests ----------------------------------------------------------------

// THE regression guard for the cross-tenant WebSocket leak: broadcast() used to
// send every event to every connected client with no filtering whatsoever, so any
// authenticated socket received the heartbeat stream of every monitor in the
// install — including monitors the user had no access to over the REST API.
//
// A client with no grants must receive NOTHING for someone else's monitor.
func TestHub_ClientWithoutGrantsReceivesNoHeartbeatsForOtherUsersMonitor(t *testing.T) {
	h := newHubRBACHarness(t)

	stranger := h.addClient(h.strangerID)
	member := h.addClient(h.memberID)

	// A heartbeat on a monitor the stranger was never granted.
	if err := h.bus.Publish(context.Background(), ports.Event{
		Type:    EventHeartbeat,
		Payload: &domain.Heartbeat{MonitorID: h.monitorGranted, Status: domain.StatusUp, Ping: 12, Time: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	strangerFrames := awaitFrames(stranger, 250*time.Millisecond)
	if got := heartbeatFramesFor(strangerFrames, h.monitorGranted); len(got) != 0 {
		t.Fatalf("a client with no grants received %d heartbeat frame(s) for another user's monitor %d: %v",
			len(got), h.monitorGranted, got)
	}

	// The control: the member, who WAS granted that monitor, must receive it —
	// otherwise this test would also pass against a hub that broadcasts nothing at
	// all, which is not a fix, it is an outage.
	memberFrames := awaitFrames(member, 250*time.Millisecond)
	if got := heartbeatFramesFor(memberFrames, h.monitorGranted); len(got) != 1 {
		t.Fatalf("the granted client received %d heartbeat frames for monitor %d; want exactly 1 (frames: %v)",
			len(got), h.monitorGranted, memberFrames)
	}
}

// A granted client must not receive events for the monitors it was NOT granted.
func TestHub_GrantedClientSeesOnlyItsOwnMonitors(t *testing.T) {
	h := newHubRBACHarness(t)
	member := h.addClient(h.memberID)

	if err := h.bus.Publish(context.Background(), ports.Event{
		Type:    EventHeartbeat,
		Payload: &domain.Heartbeat{MonitorID: h.monitorHidden, Status: domain.StatusDown, Ping: 0, Time: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	frames := awaitFrames(member, 250*time.Millisecond)
	if got := heartbeatFramesFor(frames, h.monitorHidden); len(got) != 0 {
		t.Fatalf("client received %d heartbeat frame(s) for monitor %d, which it was never granted",
			len(got), h.monitorHidden)
	}
}

// An admin still sees everything — the single-admin install must be unchanged.
func TestHub_AdminReceivesEveryMonitorsHeartbeats(t *testing.T) {
	h := newHubRBACHarness(t)
	admin := h.addClient(h.adminID)

	for _, id := range []int64{h.monitorGranted, h.monitorHidden} {
		if err := h.bus.Publish(context.Background(), ports.Event{
			Type:    EventHeartbeat,
			Payload: &domain.Heartbeat{MonitorID: id, Status: domain.StatusUp, Ping: 5, Time: time.Now().UTC()},
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	frames := awaitFrames(admin, 300*time.Millisecond)
	for _, id := range []int64{h.monitorGranted, h.monitorHidden} {
		if got := heartbeatFramesFor(frames, id); len(got) != 1 {
			t.Errorf("admin received %d heartbeat frames for monitor %d; want 1", len(got), id)
		}
	}
}

// An UNAUTHENTICATED socket (no JWT → UserID 0) must receive nothing. It used to
// receive the entire install's event stream.
func TestHub_AnonymousClientReceivesNothing(t *testing.T) {
	h := newHubRBACHarness(t)
	anon := h.addClient(0)

	if err := h.bus.Publish(context.Background(), ports.Event{
		Type:    EventHeartbeat,
		Payload: &domain.Heartbeat{MonitorID: h.monitorGranted, Status: domain.StatusUp, Ping: 7, Time: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if frames := awaitFrames(anon, 250*time.Millisecond); len(frames) != 0 {
		t.Fatalf("an unauthenticated client received %d frame(s): %v", len(frames), frames)
	}
}

// stats.update must be computed PER CLIENT from that client's visible set. It used
// to be a single install-wide count broadcast to everyone, telling every user how
// many monitors existed and how many were down.
func TestHub_StatsUpdateIsScopedPerClient(t *testing.T) {
	h := newHubRBACHarness(t)

	admin := h.addClient(h.adminID)
	member := h.addClient(h.memberID)
	stranger := h.addClient(h.strangerID)

	// A status change triggers emitStatsUpdate.
	if err := h.bus.Publish(context.Background(), ports.Event{
		Type: EventStatusChange,
		Payload: map[string]any{
			"monitor_id": h.monitorGranted,
			"new_status": domain.StatusUp,
		},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	want := map[string]float64{
		"admin":    2, // sees both monitors
		"member":   1, // granted one
		"stranger": 0, // granted none
	}
	for name, client := range map[string]*Client{"admin": admin, "member": member, "stranger": stranger} {
		// stats.update is trailing-edge debounced (statsDebounce = 250ms) and then
		// does a visibility-scoped recompute. Under -race / busy CI that can land
		// well after the immediate status.change frame; wait for the stats frame
		// explicitly instead of a short quiet window that returns after status.change.
		stats, frames := awaitFrameOfType(client, EventStatsUpdate, 2*time.Second)
		if stats == nil {
			t.Fatalf("%s received no stats.update frame (frames: %v)", name, frames)
		}
		if total, _ := stats["total"].(float64); total != want[name] {
			t.Errorf("%s stats.update total = %v; want %v — the count must come from that client's "+
				"visible set, not a global ListActive", name, stats["total"], want[name])
		}
	}
}

// awaitFrameOfType waits up to timeout for a frame of the given type and returns
// its payload plus every frame observed along the way.
func awaitFrameOfType(c *Client, typ string, timeout time.Duration) (payload map[string]any, frames []map[string]any) {
	deadline := time.After(timeout)
	for {
		select {
		case data := <-c.Outbound():
			var frame map[string]any
			if err := json.Unmarshal(data, &frame); err != nil {
				continue
			}
			frames = append(frames, frame)
			if frame["type"] == typ {
				payload, _ = frame["payload"].(map[string]any)
				return payload, frames
			}
		case <-deadline:
			return nil, frames
		}
	}
}
