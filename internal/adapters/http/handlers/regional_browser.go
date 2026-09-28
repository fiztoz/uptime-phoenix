package handlers

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RegionalBrowserPublisher coalesces committed invalidations into explicit
// browser Views. Each API replica emits only to its own hub. No full monitor
// inventory query runs for a heartbeat, and all queued work is bounded.
type RegionalBrowserPublisher struct {
	regional     *services.MonitorRegionalService
	fleet        *services.ProbeFleetService
	observations ports.RegionalBrowserRepository
	bus          ports.EventBus
	emit         func(ports.Event)
	active       func() int
}

// NewRegionalBrowserPublisher uses the same view mappers as HTTP reads.
func NewRegionalBrowserPublisher(regional *services.MonitorRegionalService, fleet *services.ProbeFleetService, observations ports.RegionalBrowserRepository, bus ports.EventBus, emit func(ports.Event), active func() int) *RegionalBrowserPublisher {
	return &RegionalBrowserPublisher{regional: regional, fleet: fleet, observations: observations, bus: bus, emit: emit, active: active}
}

// OnRegionalEvidence publishes only identities after commit, safe across Redis.
func (p *RegionalBrowserPublisher) OnRegionalEvidence(ctx context.Context, ids []int64) {
	for _, id := range ids {
		_ = p.bus.Publish(ctx, ports.Event{Type: "monitor.regional.invalidate", Payload: map[string]any{"monitor_id": id}})
	}
}

// Run owns one bounded queue until the application context closes. Expiry
// refreshes allow UNKNOWN to arrive even when a disconnected source is silent.
func (p *RegionalBrowserPublisher) Run(ctx context.Context) {
	beats := p.bus.Subscribe("heartbeat")
	changes := p.bus.Subscribe("monitor.update")
	invalidations := p.bus.Subscribe("monitor.regional.invalidate")
	watches := p.bus.Subscribe("monitor.regional.watch")
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	fleetTick := time.NewTicker(5 * time.Second)
	defer fleetTick.Stop()
	pending := make(map[int64]bool)
	next := make(map[int64]time.Time)
	last := make(map[string]string)
	queue := func(event ports.Event) {
		id := browserMonitorID(event.Payload)
		if id > 0 && len(pending) < 10000 {
			pending[id] = true
		}
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
			if _, tracked := next[id]; !tracked {
				queue(event)
			}
		case event, ok := <-invalidations:
			if !ok {
				return
			}
			queue(event)
		case now := <-tick.C:
			if p.active() == 0 {
				clear(pending)
				clear(next)
				clear(last)
				continue
			}
			for id, due := range next {
				if !now.Before(due) {
					pending[id] = true
					delete(next, id)
				}
			}
			processed := 0
			for id := range pending {
				delete(pending, id)
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				due := p.publishMonitor(readCtx, id, last)
				cancel()
				if !due.IsZero() && len(next) < 10000 {
					next[id] = due
				}
				processed++
				if processed >= 32 {
					break
				}
			}
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

func (p *RegionalBrowserPublisher) publishMonitor(ctx context.Context, id int64, last map[string]string) time.Time {
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
	rows, err := p.observations.LatestObservations(ctx, id)
	if err == nil {
		for _, row := range rows {
			view := toRegionalHeartbeatView(&row)
			p.emitChanged("monitor.probe.heartbeat", row.ProbeID+"/"+strconv.FormatInt(id, 10), view, last)
		}
	}
	return next
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
