package handlers

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RegionalBrowserPublisher coalesces committed invalidations into explicit
// browser Views. Each API replica emits only to its own hub. No full monitor
// inventory query runs for a heartbeat, and all queued work is bounded.
//
// Work runs on two lanes, each on its own goroutine. The heartbeat lane reads
// only the latest persisted samples and emits monitor.probe.heartbeat; it
// drains completely every tick. The health lane rebuilds monitor.health
// (current health, 24 h coverage and probe diagnostics), which costs several
// queries per monitor, so it keeps a per-tick budget. Both lanes are FIFO, so
// the oldest invalidation goes first. The heartbeat lane used to wait behind
// health rebuilds, which made heartbeat fan-out latency equal the health
// backlog.
type RegionalBrowserPublisher struct {
	regional     *services.MonitorRegionalService
	fleet        *services.ProbeFleetService
	observations ports.RegionalBrowserRepository
	bus          ports.EventBus
	emit         func(ports.Event)
	active       func() int

	// Lane work. Tests replace these to observe ordering and lane independence.
	publishHealthFn func(ctx context.Context, id int64, last map[string]string) time.Time
	publishBeatsFn  func(ctx context.Context, id int64, last map[string]string)
}

// browserQueueLimit bounds each lane's pending monitors.
const browserQueueLimit = 10000

// browserHealthBudget is the number of monitor.health rebuilds per tick.
const browserHealthBudget = 32

// NewRegionalBrowserPublisher uses the same view mappers as HTTP reads.
func NewRegionalBrowserPublisher(regional *services.MonitorRegionalService, fleet *services.ProbeFleetService, observations ports.RegionalBrowserRepository, bus ports.EventBus, emit func(ports.Event), active func() int) *RegionalBrowserPublisher {
	p := &RegionalBrowserPublisher{regional: regional, fleet: fleet, observations: observations, bus: bus, emit: emit, active: active}
	p.publishHealthFn = p.publishHealth
	p.publishBeatsFn = p.publishBeats
	return p
}

// monitorQueue is a FIFO set of monitor IDs: a monitor queued again before it
// is processed keeps its original place.
type monitorQueue struct {
	order  []int64
	queued map[int64]bool
}

func newMonitorQueue() *monitorQueue { return &monitorQueue{queued: make(map[int64]bool)} }

func (q *monitorQueue) push(id int64) {
	if id <= 0 || q.queued[id] || len(q.order) >= browserQueueLimit {
		return
	}
	q.queued[id] = true
	q.order = append(q.order, id)
}

func (q *monitorQueue) pop() (int64, bool) {
	if len(q.order) == 0 {
		return 0, false
	}
	id := q.order[0]
	q.order[0] = 0
	q.order = q.order[1:]
	delete(q.queued, id)
	if len(q.order) == 0 {
		q.order = nil // release the backing array once drained
	}
	return id, true
}

func (q *monitorQueue) len() int { return len(q.order) }

func (q *monitorQueue) reset() {
	q.order = nil
	clear(q.queued)
}

// OnRegionalEvidence publishes only identities after commit, safe across Redis.
func (p *RegionalBrowserPublisher) OnRegionalEvidence(ctx context.Context, ids []int64) {
	for _, id := range ids {
		_ = p.bus.Publish(ctx, ports.Event{Type: "monitor.regional.invalidate", Payload: map[string]any{"monitor_id": id}})
	}
}

// healthLane is the health lane's queue and freshness deadlines. Run adds
// work to it; runHealth is the only goroutine that rebuilds health.
type healthLane struct {
	mu    sync.Mutex
	queue *monitorQueue
	next  map[int64]time.Time
}

func newHealthLane() *healthLane {
	return &healthLane{queue: newMonitorQueue(), next: make(map[int64]time.Time)}
}

func (l *healthLane) push(id int64) {
	l.mu.Lock()
	l.queue.push(id)
	l.mu.Unlock()
}

// tracked reports whether id already has a freshness deadline.
func (l *healthLane) tracked(id int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.next[id]
	return ok
}

func (l *healthLane) reset() {
	l.mu.Lock()
	l.queue.reset()
	clear(l.next)
	l.mu.Unlock()
}

// Run owns the heartbeat lane until the application context closes, and
// starts the health lane on its own goroutine. Expiry refreshes allow UNKNOWN
// to arrive even when a disconnected source is silent.
//
// The lanes used to share this goroutine, so a heartbeat whose invalidation
// arrived while a tick was rebuilding health waited for the rest of that tick's
// health budget. Under load the budget took seconds, and that wait became the
// WebSocket fan-out latency. Now the heartbeat lane drains every tick no matter
// how long health takes.
//
// The two lanes are independent, so for one monitor a monitor.probe.heartbeat
// frame and a monitor.health frame can reach a client in either order. The
// browser does not rely on that order: monitor.health is accepted only by
// projection_version and as_of, and heartbeat rows are merged by observation
// time (see docs/multi-region/PROTOCOL.md section 8).
func (p *RegionalBrowserPublisher) Run(ctx context.Context) {
	beats := p.bus.Subscribe("heartbeat")
	changes := p.bus.Subscribe("monitor.update")
	invalidations := p.bus.Subscribe("monitor.regional.invalidate")
	watches := p.bus.Subscribe("monitor.regional.watch")
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	fleetTick := time.NewTicker(5 * time.Second)
	defer fleetTick.Stop()
	beatsQ := newMonitorQueue()
	health := newHealthLane()
	last := make(map[string]string)
	healthDone := make(chan struct{})
	go func() {
		defer close(healthDone)
		p.runHealth(ctx, health)
	}()
	defer func() { <-healthDone }()
	queue := func(event ports.Event) {
		id := browserMonitorID(event.Payload)
		beatsQ.push(id)
		health.push(id)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-beats:
			if !ok {
				return
			}
			queue(event)
		case event, ok := <-changes:
			if !ok {
				return
			}
			queue(event)
		case event, ok := <-watches:
			if !ok {
				return
			}
			id := browserMonitorID(event.Payload)
			if !health.tracked(id) {
				queue(event)
			}
		case event, ok := <-invalidations:
			if !ok {
				return
			}
			queue(event)
		case <-tick.C:
			if p.active() == 0 {
				beatsQ.reset()
				clear(last)
				continue
			}
			p.drainBeats(ctx, beatsQ, last)
			if len(last) > 50000 {
				clear(last)
			}
		case <-fleetTick.C:
			if p.active() > 0 {
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				p.publishFleet(readCtx, last)
				cancel()
			}
		}
	}
}

// runHealth rebuilds health on its own ticker until ctx closes.
func (p *RegionalBrowserPublisher) runHealth(ctx context.Context, lane *healthLane) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			if p.active() == 0 {
				lane.reset()
				continue
			}
			p.drainHealth(ctx, now, lane)
		}
	}
}

// drainBeats publishes every queued heartbeat. A heartbeat row cannot change
// without new evidence, which arrives as an invalidation, so this lane has no
// deadlines and no budget.
func (p *RegionalBrowserPublisher) drainBeats(ctx context.Context, beatsQ *monitorQueue, last map[string]string) {
	for beatsQ.len() > 0 {
		id, _ := beatsQ.pop()
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		p.publishBeatsFn(readCtx, id, last)
		cancel()
	}
}

// drainHealth runs one health tick: expired freshness deadlines join the
// queue, oldest first, then at most browserHealthBudget monitors are rebuilt.
func (p *RegionalBrowserPublisher) drainHealth(ctx context.Context, now time.Time, lane *healthLane) {
	lane.mu.Lock()
	due := make([]int64, 0)
	for id, at := range lane.next {
		if !now.Before(at) {
			due = append(due, id)
		}
	}
	// Oldest deadline first, so refreshes are as fair as invalidations.
	slices.SortFunc(due, func(a, b int64) int { return lane.next[a].Compare(lane.next[b]) })
	for _, id := range due {
		delete(lane.next, id)
		lane.queue.push(id)
	}
	lane.mu.Unlock()
	for processed := 0; processed < browserHealthBudget; processed++ {
		lane.mu.Lock()
		id, ok := lane.queue.pop()
		lane.mu.Unlock()
		if !ok {
			break
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		at := p.publishHealthFn(readCtx, id, nil)
		cancel()
		if at.IsZero() {
			continue
		}
		lane.mu.Lock()
		if len(lane.next) < browserQueueLimit {
			lane.next[id] = at
		}
		lane.mu.Unlock()
	}
}

func browserMonitorID(payload any) int64 {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	var row struct {
		ID              int64
		MonitorID       int64 `json:"monitor_id"`
		DomainMonitorID int64 `json:"MonitorID"`
		Heartbeat       *struct{ MonitorID int64 }
	}
	if json.Unmarshal(data, &row) != nil {
		return 0
	}
	if row.Heartbeat != nil {
		return row.Heartbeat.MonitorID
	}
	if row.MonitorID > 0 {
		return row.MonitorID
	}
	if row.DomainMonitorID > 0 {
		return row.DomainMonitorID
	}
	return row.ID
}

// publishHealth emits monitor.health and monitor.probe.status for one monitor
// and returns when its freshness next needs re-evaluating.
func (p *RegionalBrowserPublisher) publishHealth(ctx context.Context, id int64, _ map[string]string) time.Time {
	now := time.Now().UTC()
	result, err := p.regional.HealthForPublication(ctx, id, 24, now)
	if err != nil {
		return time.Time{}
	}
	view, err := toMonitorHealthView(result)
	if err != nil {
		return time.Time{}
	}
	p.emit(ports.Event{Type: "monitor.health", Payload: view})
	next := now.Add(30 * time.Second)
	for _, region := range view.Regions {
		if region.FreshUntil != nil && region.FreshUntil.After(now) && region.FreshUntil.Before(next) {
			next = *region.FreshUntil
		}
		p.emit(ports.Event{Type: "monitor.probe.status", Payload: map[string]any{
			"monitor_id": id, "probe_id": region.ProbeID, "status": region.Status,
			"connection_status": region.ConnectionStatus, "observed_at": region.ObservedAt,
			"received_at": region.ReceivedAt, "fresh_until": region.FreshUntil, "reason": region.Reason,
		}})
	}
	return next
}

// publishBeats emits monitor.probe.heartbeat for samples not yet sent.
func (p *RegionalBrowserPublisher) publishBeats(ctx context.Context, id int64, last map[string]string) {
	rows, err := p.observations.LatestObservations(ctx, id)
	if err != nil {
		return
	}
	for _, row := range rows {
		view := toRegionalHeartbeatView(&row)
		p.emitChanged("monitor.probe.heartbeat", row.ProbeID+"/"+strconv.FormatInt(id, 10), view, last)
	}
}

func (p *RegionalBrowserPublisher) publishFleet(ctx context.Context, last map[string]string) {
	cursor := ""
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := p.fleet.List(ctx, cursor, 100, time.Now().UTC())
		if err != nil {
			return
		}
		for _, entry := range page.Items {
			view := toProbeView(entry)
			p.emitChanged("probe.status", view.ID, map[string]any{
				"id": view.ID, "key": view.Key, "name": view.Name, "location": view.Location, "kind": view.Kind,
				"enabled": view.Enabled, "enrollment_state": view.EnrollmentState, "connection_status": view.ConnectionStatus,
				"execution_status": view.ExecutionStatus, "last_seen_at": view.LastSeenAt, "revision": strconv.FormatInt(view.Revision, 10),
			}, last)
			if entry.Summary.ConfigSyncStatus != "" {
				p.emitChanged("probe.config.status", view.ID, map[string]any{"probe_id": view.ID, "revision": view.DesiredConfigRevision, "status": entry.Summary.ConfigSyncStatus, "errors": []string{}}, last)
			}
		}
		if page.NextCursor == nil {
			return
		}
		cursor = *page.NextCursor
	}
}

func (p *RegionalBrowserPublisher) emitChanged(kind, key string, payload any, last map[string]string) {
	encoded, err := json.Marshal(payload)
	if err != nil || last[kind+key] == string(encoded) {
		return
	}
	last[kind+key] = string(encoded)
	p.emit(ports.Event{Type: kind, Payload: payload})
}

// SetBrowserPublisher refreshes a newly viewed monitor even during a silent partition.
func (h *MonitorRegionalHandlers) SetBrowserPublisher(publisher *RegionalBrowserPublisher) {
	h.browser = publisher
}

// ObserveMonitor schedules the first snapshot without feeding HTTP refreshes back into events.
func (p *RegionalBrowserPublisher) ObserveMonitor(ctx context.Context, id int64) {
	_ = p.bus.Publish(ctx, ports.Event{Type: "monitor.regional.watch", Payload: map[string]any{"monitor_id": id}})
}
