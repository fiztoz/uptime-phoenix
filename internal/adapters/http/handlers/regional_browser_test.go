package handlers

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type laneRecorder struct {
	health []int64
	beats  []int64
}

func newLanePublisher(rec *laneRecorder, nextAt time.Time) *RegionalBrowserPublisher {
	p := NewRegionalBrowserPublisher(nil, nil, nil, nil, nil, func() int { return 1 })
	p.publishHealthFn = func(_ context.Context, id int64, _ map[string]string) time.Time {
		rec.health = append(rec.health, id)
		return nextAt
	}
	p.publishBeatsFn = func(_ context.Context, id int64, _ map[string]string) {
		rec.beats = append(rec.beats, id)
	}
	return p
}

func idsUpTo(n int64) []int64 {
	ids := make([]int64, 0, n)
	for id := int64(1); id <= n; id++ {
		ids = append(ids, id)
	}
	return ids
}

// A burst of invalidations (a probe's whole assignment set) must not delay
// heartbeat fan-out behind the expensive health rebuilds. Every queued
// monitor's heartbeat goes out on the first tick; health keeps its budget.
func TestRegionalBrowser_HeartbeatLaneDoesNotWaitForHealthBacklog(t *testing.T) {
	rec := &laneRecorder{}
	p := newLanePublisher(rec, time.Time{})
	beatsQ, health := newMonitorQueue(), newHealthLane()
	for _, id := range idsUpTo(200) {
		beatsQ.push(id)
		health.push(id)
	}

	p.drainBeats(context.Background(), beatsQ, map[string]string{})
	p.drainHealth(context.Background(), time.Now(), health)

	if !slices.Equal(rec.beats, idsUpTo(200)) {
		t.Fatalf("heartbeat lane published %d monitors on the first tick, want all 200 in order", len(rec.beats))
	}
	if len(rec.health) != browserHealthBudget || health.queue.len() != 200-browserHealthBudget {
		t.Fatalf("health lane ran %d (left %d), want budget %d", len(rec.health), health.queue.len(), browserHealthBudget)
	}
}

// The health lane is FIFO across ticks. With random map order a monitor could
// wait for many ticks while later arrivals went first.
func TestRegionalBrowser_HealthLaneIsFIFOAcrossTicks(t *testing.T) {
	rec := &laneRecorder{}
	p := newLanePublisher(rec, time.Time{})
	health := newHealthLane()
	for _, id := range idsUpTo(100) {
		health.push(id)
	}
	health.push(5) // already queued: keeps its original place
	for tick := 0; tick < 4; tick++ {
		p.drainHealth(context.Background(), time.Now(), health)
	}
	if !slices.Equal(rec.health, idsUpTo(100)) {
		t.Fatalf("health order = %v, want 1..100 once each", rec.health)
	}
}

// An expired freshness deadline re-evaluates health (so UNKNOWN can appear for
// a silent source) but does not re-read heartbeat rows, which only change with
// new evidence.
func TestRegionalBrowser_FreshnessRefreshOnlyRunsHealthLane(t *testing.T) {
	rec := &laneRecorder{}
	now := time.Now()
	p := newLanePublisher(rec, now.Add(time.Minute))
	health := newHealthLane()
	health.next = map[int64]time.Time{7: now.Add(-time.Second), 3: now.Add(-2 * time.Second), 9: now.Add(time.Hour)}
	next := health.next

	p.drainHealth(context.Background(), now, health)

	if !slices.Equal(rec.health, []int64{3, 7}) {
		t.Fatalf("refreshed %v, want the two expired deadlines oldest first [3 7]", rec.health)
	}
	if len(rec.beats) != 0 {
		t.Fatalf("freshness refresh re-read heartbeats for %v", rec.beats)
	}
	if _, ok := next[9]; !ok || !next[3].Equal(now.Add(time.Minute)) {
		t.Fatalf("deadlines not rescheduled: %v", next)
	}
}

func TestMonitorQueue_BoundedAndDeduplicated(t *testing.T) {
	q := newMonitorQueue()
	for id := int64(0); id <= browserQueueLimit+10; id++ {
		q.push(id)
		q.push(id)
	}
	if q.len() != browserQueueLimit {
		t.Fatalf("queue length %d, want limit %d", q.len(), browserQueueLimit)
	}
	if id, _ := q.pop(); id != 1 {
		t.Fatalf("first id %d, want 1 (ids <= 0 are ignored)", id)
	}
	q.reset()
	if _, ok := q.pop(); ok || q.len() != 0 {
		t.Fatal("reset left work queued")
	}
}

// The heartbeat lane must not wait for a health lane that is already running.
// drain used to run both lanes on Run's goroutine, so an invalidation that
// arrived while a tick was rebuilding health waited for the rest of that
// tick's budget (32 rebuilds of several queries each) before its heartbeat
// went out. On the M6 F2 hub that wait was the WebSocket fan-out p95: 2.9 s in
// F4b after warm-up and 1.97 s in F4c.
func TestRegionalBrowser_HeartbeatDoesNotWaitForRunningHealthLane(t *testing.T) {
	const slowHealth = 40 * time.Millisecond
	bus := eventbus.NewMemoryBus()
	t.Cleanup(bus.Close)
	p := NewRegionalBrowserPublisher(nil, nil, nil, bus, nil, func() int { return 1 })
	var mu sync.Mutex
	healthCalls := 0
	beatAt := make(map[int64]time.Time)
	p.publishHealthFn = func(_ context.Context, _ int64, _ map[string]string) time.Time {
		mu.Lock()
		healthCalls++
		mu.Unlock()
		time.Sleep(slowHealth)
		return time.Time{}
	}
	p.publishBeatsFn = func(_ context.Context, id int64, _ map[string]string) {
		mu.Lock()
		if _, seen := beatAt[id]; !seen {
			beatAt[id] = time.Now()
		}
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	invalidate := func(id int64) {
		_ = bus.Publish(context.Background(), ports.Event{Type: "monitor.regional.invalidate", Payload: map[string]any{"monitor_id": id}})
	}
	beatSeen := func(id int64) (time.Time, bool) {
		mu.Lock()
		defer mu.Unlock()
		at, ok := beatAt[id]
		return at, ok
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	// Run subscribes asynchronously; repeat the first invalidation until the
	// publisher has seen it.
	waitFor("publisher to subscribe", func() bool {
		invalidate(1)
		_, ok := beatSeen(1)
		return ok
	})
	waitFor("first health rebuild to finish", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return healthCalls >= 1
	})

	// Two ticks' worth of health work (64 monitors at 40 ms each).
	for id := int64(2); id <= 65; id++ {
		invalidate(id)
	}
	waitFor("health lane to start on the burst", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return healthCalls >= 3
	})

	sent := time.Now()
	invalidate(1000)
	waitFor("heartbeat for monitor 1000", func() bool {
		_, ok := beatSeen(1000)
		return ok
	})
	at, _ := beatSeen(1000)
	if waited := at.Sub(sent); waited > 500*time.Millisecond {
		t.Fatalf("heartbeat published %v after its invalidation while the health lane was busy; want under 500 ms (one tick plus slack)", waited)
	}
}
