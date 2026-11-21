package handlers

import (
	"context"
	"slices"
	"testing"
	"time"
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
	beatsQ, healthQ := newMonitorQueue(), newMonitorQueue()
	for _, id := range idsUpTo(200) {
		beatsQ.push(id)
		healthQ.push(id)
	}

	p.drain(context.Background(), time.Now(), beatsQ, healthQ, map[int64]time.Time{}, map[string]string{})

	if !slices.Equal(rec.beats, idsUpTo(200)) {
		t.Fatalf("heartbeat lane published %d monitors on the first tick, want all 200 in order", len(rec.beats))
	}
	if len(rec.health) != browserHealthBudget || healthQ.len() != 200-browserHealthBudget {
		t.Fatalf("health lane ran %d (left %d), want budget %d", len(rec.health), healthQ.len(), browserHealthBudget)
	}
}

// The health lane is FIFO across ticks. With random map order a monitor could
// wait for many ticks while later arrivals went first.
func TestRegionalBrowser_HealthLaneIsFIFOAcrossTicks(t *testing.T) {
	rec := &laneRecorder{}
	p := newLanePublisher(rec, time.Time{})
	beatsQ, healthQ := newMonitorQueue(), newMonitorQueue()
	for _, id := range idsUpTo(100) {
		healthQ.push(id)
	}
	healthQ.push(5) // already queued: keeps its original place
	for tick := 0; tick < 4; tick++ {
		p.drain(context.Background(), time.Now(), beatsQ, healthQ, map[int64]time.Time{}, map[string]string{})
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
	beatsQ, healthQ := newMonitorQueue(), newMonitorQueue()
	next := map[int64]time.Time{7: now.Add(-time.Second), 3: now.Add(-2 * time.Second), 9: now.Add(time.Hour)}

	p.drain(context.Background(), now, beatsQ, healthQ, next, map[string]string{})

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
