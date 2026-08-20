package ws

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// This file is the R3.6 regression guard.
//
// The bug was not a wrong value anywhere — every test in hub_rbac_test.go passed
// while it was live. It was a COST: emitStatsUpdate ran inline in the hub's
// single fan-out goroutine and issued one heartbeat query per active monitor,
// once per status.change, and heartbeat_service publishes status.change on the
// first check of EVERY monitor. N monitors coming up therefore cost ~N² serialized
// database round trips, heartbeats queued behind that work, and the WebSocket
// event p95 blew past its 1 s target at only 100 monitors.
//
// So these tests assert the effect that actually regressed: the number of
// repository round trips, and the number of recomputes, for a burst of N status
// changes. Asserting "an event arrived" would have stayed green throughout.
//
// Deliberately written against only the hub API that existed BEFORE the fix, so
// it compiles against the old implementation and its failure there is real.

// --- Counting test doubles -------------------------------------------------

// countingHeartbeatRepo implements ports.HeartbeatRepository AND
// ports.HeartbeatBatchReader, and counts round trips of each kind separately.
type countingHeartbeatRepo struct {
	mu     sync.Mutex
	latest map[int64]*domain.Heartbeat

	// singleCalls counts GetLatest calls — one per monitor under the old N+1.
	singleCalls atomic.Int64
	// batchCalls counts GetLatestForMonitors calls — one per recompute (per 500
	// monitors) under the fix.
	batchCalls atomic.Int64
	// batchIDs counts monitor ids passed through the batch path, so a "batch"
	// implementation that secretly looped could not hide behind a low call count.
	batchIDs atomic.Int64
}

func newCountingHeartbeatRepo() *countingHeartbeatRepo {
	return &countingHeartbeatRepo{latest: make(map[int64]*domain.Heartbeat)}
}

// roundTrips is the metric under test: how many times the hub went to the
// database for heartbeat state, by any route.
func (r *countingHeartbeatRepo) roundTrips() int64 {
	return r.singleCalls.Load() + r.batchCalls.Load()
}

func (r *countingHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latest[h.MonitorID] = h
	return nil
}

func (r *countingHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.singleCalls.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	hb, ok := r.latest[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return hb, nil
}

func (r *countingHeartbeatRepo) GetLatestForMonitors(_ context.Context, ids []int64) (map[int64]*domain.Heartbeat, error) {
	r.batchCalls.Add(1)
	r.batchIDs.Add(int64(len(ids)))
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[int64]*domain.Heartbeat, len(ids))
	for _, id := range ids {
		if hb, ok := r.latest[id]; ok {
			out[id] = hb
		}
	}
	return out, nil
}

func (r *countingHeartbeatRepo) ListByMonitor(_ context.Context, _ int64, _, _ time.Time) ([]*domain.Heartbeat, error) {
	return nil, nil
}
func (r *countingHeartbeatRepo) DeleteByMonitor(_ context.Context, _ int64) error     { return nil }
func (r *countingHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error { return nil }
func (r *countingHeartbeatRepo) SaveAggregate1m(_ context.Context, _ *ports.Aggregate1m) error {
	return nil
}
func (r *countingHeartbeatRepo) SaveAggregate1h(_ context.Context, _ *ports.Aggregate1h) error {
	return nil
}
func (r *countingHeartbeatRepo) SaveAggregate1d(_ context.Context, _ *ports.Aggregate1d) error {
	return nil
}
func (r *countingHeartbeatRepo) GetAggregate1m(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *countingHeartbeatRepo) GetAggregate1h(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *countingHeartbeatRepo) GetAggregate1d(_ context.Context, _ int64, _ time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

// --- Harness ---------------------------------------------------------------

type eventPathHarness struct {
	hub        *Hub
	bus        *eventbus.MemoryBus
	heartbeats *countingHeartbeatRepo
	adminID    int64
	monitorIDs []int64
}

// newEventPathHarness builds a hub over n active monitors owned by an admin, so
// visibility resolution is trivial and the counts under test are the hub's
// heartbeat lookups rather than access-control noise.
func newEventPathHarness(t *testing.T, n int) *eventPathHarness {
	t.Helper()
	ctx := context.Background()

	monitors := newHubFakeMonitorRepo()
	heartbeats := newCountingHeartbeatRepo()

	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		m := &domain.Monitor{UserID: 1, Name: "m", Type: "http", Active: true, Interval: 60}
		if err := monitors.Create(ctx, m); err != nil {
			t.Fatalf("create monitor %d: %v", i, err)
		}
		if err := heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: m.ID, Status: domain.StatusUp, Time: time.Now().UTC()}); err != nil {
			t.Fatalf("seed heartbeat %d: %v", i, err)
		}
		ids = append(ids, m.ID)
	}

	users := memory.NewUserRepo()
	admin := &domain.User{Username: "admin", Active: true, IsAdmin: true}
	if err := users.Create(ctx, admin); err != nil {
		t.Fatalf("create admin: %v", err)
	}
	perms := memory.NewUserPermissionRepo()
	access := services.NewAccessService(users, perms, nil, monitors)

	bus := eventbus.NewMemoryBus()
	t.Cleanup(bus.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := NewHub(bus, monitors, heartbeats, access, nil, log)
	if !hub.waitReady(2 * time.Second) {
		t.Fatal("hub never subscribed to the event bus")
	}

	// Reset counters so seeding does not contribute to the measurement.
	heartbeats.singleCalls.Store(0)
	heartbeats.batchCalls.Store(0)
	heartbeats.batchIDs.Store(0)

	return &eventPathHarness{
		hub:        hub,
		bus:        bus,
		heartbeats: heartbeats,
		adminID:    admin.ID,
		monitorIDs: ids,
	}
}

// publishStatusChangeBurst publishes one status.change per monitor, the shape
// heartbeat_service emits on every monitor's first check — i.e. exactly what a
// cold start or a ramp produces.
func (h *eventPathHarness) publishStatusChangeBurst(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, id := range h.monitorIDs {
		if err := h.bus.Publish(ctx, ports.Event{
			Type: EventStatusChange,
			Payload: map[string]any{
				"monitor_id": id,
				"old_status": domain.StatusPending,
				"new_status": domain.StatusUp,
			},
		}); err != nil {
			t.Fatalf("publish status.change for %d: %v", id, err)
		}
	}
}

// --- Tests -----------------------------------------------------------------

// A burst of N status changes must not cost O(N²) heartbeat round trips.
//
// Old behavior: each of the N events ran a full recompute over all N monitors,
// one GetLatest per monitor per event → ~N² round trips (10,000 at N=100).
// New behavior: the recompute is debounced off the fan-out path and uses a
// single batched lookup → a small constant number of round trips.
//
// The bound is deliberately generous — it is not calibrating the debounce, it is
// separating "linear-ish" from "quadratic". At N=100 the old code exceeds it by
// two orders of magnitude.
func TestHub_StatusChangeBurstDoesNotScaleQuadratically(t *testing.T) {
	const n = 100
	h := newEventPathHarness(t, n)

	client := NewClient("burst-client", h.adminID)
	h.hub.AddClient(client)
	// Drain frames continuously so a full client buffer cannot mask the cost by
	// short-circuiting fan-out.
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-client.Outbound():
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	h.publishStatusChangeBurst(t)

	// Let the burst drain and any trailing debounced recompute land.
	time.Sleep(2 * time.Second)

	got := h.heartbeats.roundTrips()
	quadratic := int64(n) * int64(n)

	if got >= quadratic/10 {
		t.Fatalf("heartbeat round trips for a burst of %d status changes = %d; "+
			"that is the O(N²) fan-out R3.6 was about (N²=%d). "+
			"Expected a debounced, batched recompute costing a small constant.",
			n, got, quadratic)
	}

	// Tighter assertion on the intended design: a handful of recomputes at most.
	if got > int64(n)/2 {
		t.Fatalf("heartbeat round trips = %d for %d monitors; expected far fewer than N/2=%d "+
			"(one batched lookup per debounced recompute)", got, n, n/2)
	}

	t.Logf("MEASURED: roundTrips=%d batchCalls=%d batchIDs=%d singleCalls=%d (N=%d, N^2=%d)",
		got, h.heartbeats.batchCalls.Load(), h.heartbeats.batchIDs.Load(),
		h.heartbeats.singleCalls.Load(), n, quadratic)

	// The batched path must actually be the one carrying the work; a low count
	// achieved by simply not resolving statuses would be a different bug.
	if h.heartbeats.batchCalls.Load() == 0 {
		t.Fatalf("no batched heartbeat lookup was used; single-id calls=%d. "+
			"The hub must prefer ports.HeartbeatBatchReader when the repository offers it",
			h.heartbeats.singleCalls.Load())
	}
	if h.heartbeats.singleCalls.Load() > int64(n)/2 {
		t.Fatalf("per-monitor GetLatest calls = %d; the N+1 path is still live", h.heartbeats.singleCalls.Load())
	}
}

// stats.update is a badge counter, so a storm of status changes must collapse
// into a small number of recomputes rather than one per event.
//
// This asserts the coalescing directly, independent of round-trip counts: under
// the old code the hub emitted one stats.update per status.change.
func TestHub_StatsUpdateIsCoalescedUnderBurst(t *testing.T) {
	const n = 100
	h := newEventPathHarness(t, n)

	client := NewClient("coalesce-client", h.adminID)
	h.hub.AddClient(client)

	var statsFrames atomic.Int64
	done := make(chan struct{})
	go func() {
		for {
			select {
			case data := <-client.Outbound():
				// Cheap substring check avoids a JSON decode per frame.
				if len(data) > 0 && containsType(data, EventStatsUpdate) {
					statsFrames.Add(1)
				}
			case <-done:
				return
			}
		}
	}()

	h.publishStatusChangeBurst(t)
	time.Sleep(2 * time.Second)
	close(done)

	got := statsFrames.Load()
	if got == 0 {
		t.Fatal("no stats.update frame was emitted at all; the badge would never update")
	}
	if got > int64(n)/4 {
		t.Fatalf("stats.update frames for %d status changes = %d; expected a coalesced handful, "+
			"not one recompute per event", n, got)
	}
}

// Pushing the initial monitor.list on connect must not cost one heartbeat query
// per monitor, per client.
//
// This was the SECOND instance of the same N+1, and it survived the first fix:
// with fan-out repaired, 1,000 monitors × 50 connecting clients still meant
// 50,000 serialized GetLatest calls, and WebSocket connect p95 measured 1.06 s
// against a 1 s threshold. Fan-out latency and connect latency are separate
// thresholds and needed separate fixes.
func TestHub_MonitorListOnConnectUsesBatchedLookup(t *testing.T) {
	const n = 100
	const clients = 10
	h := newEventPathHarness(t, n)

	for i := 0; i < clients; i++ {
		client := NewClient("connect-client", h.adminID)
		h.hub.AddClient(client)
		h.hub.sendMonitorList(context.Background(), client)
	}

	single := h.heartbeats.singleCalls.Load()
	batch := h.heartbeats.batchCalls.Load()

	t.Logf("MEASURED: %d clients × %d monitors → singleCalls=%d batchCalls=%d", clients, n, single, batch)

	// Old behavior: n per client = 1,000 single-id calls.
	if single > int64(clients) {
		t.Fatalf("monitor.list issued %d per-monitor GetLatest calls for %d clients × %d monitors; "+
			"expected a batched lookup per client, not an N+1 per connect", single, clients, n)
	}
	if batch == 0 {
		t.Fatal("monitor.list did not use the batched heartbeat lookup at all")
	}
	if batch > int64(clients) {
		t.Fatalf("monitor.list issued %d batch calls for %d clients; expected at most one per connect", batch, clients)
	}
}

// containsType reports whether a wire frame declares the given event type.
func containsType(data []byte, eventType string) bool {
	needle := `"type":"` + eventType + `"`
	return indexOf(string(data), needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
