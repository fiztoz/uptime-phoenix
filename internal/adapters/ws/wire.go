package ws

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// wireEvent is the JSON shape expected by the Svelte frontend.
type wireEvent struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// MonitorTagView is one tag as it appears on a monitor over the wire: the tag
// definition joined with this monitor's value for it. `id` is the TAG's id — the
// same shape the REST API's MonitorView.Tags uses, so the frontend can hydrate a
// monitor from either source with one code path.
type MonitorTagView struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Value string `json:"value"`
}

// MonitorView is the WebSocket wire representation of a monitor.
//
// Tags is ALWAYS a non-nil slice (marshals as [], never null) — matching the REST
// shape, so the dashboard's tag filter never has to null-check.
type MonitorView struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// InheritGroupOwner / EffectiveOwner mirror REST; EffectiveOwner is best-effort
	// on the wire (equals Owner unless the hub resolved groups).
	InheritGroupOwner bool           `json:"inherit_group_owner"`
	EffectiveOwner    string         `json:"effective_owner"`
	Type              string         `json:"type"`
	Status            string         `json:"status"`
	Target            string         `json:"target,omitempty"`
	Config            map[string]any `json:"config"`
	Active            bool           `json:"active"`
	Interval          int            `json:"interval"`
	Timeout           float64        `json:"timeout"`
	// GroupID files this monitor under a MonitorGroup (folder). nil means
	// top-level (not in any group). Replaces the old ParentID, which nested
	// a monitor under another *monitor*.
	GroupID *int64 `json:"group_id,omitempty"`
	// Weight is the manual display order (lower first). Matches REST MonitorView.
	Weight    int              `json:"weight"`
	Tags      []MonitorTagView `json:"tags"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
}

// toWireTagViews projects the service-level tag details onto the wire shape,
// always returning a non-nil slice.
func toWireTagViews(details []services.MonitorTagDetail) []MonitorTagView {
	out := make([]MonitorTagView, 0, len(details))
	for _, d := range details {
		out = append(out, MonitorTagView{ID: d.TagID, Name: d.Name, Color: d.Color, Value: d.Value})
	}
	return out
}

// HeartbeatView is the WebSocket wire representation of a heartbeat.
type HeartbeatView struct {
	MonitorID int64  `json:"monitor_id"`
	Status    string `json:"status"`
	Time      string `json:"time"`
	Ping      int    `json:"ping"`
	Msg       string `json:"msg,omitempty"`
}

// MonitorConditionView is the WebSocket wire representation of one latest
// auxiliary condition. Notification cursor fields never cross the wire.
type MonitorConditionView struct {
	MonitorID     int64                 `json:"monitor_id"`
	Kind          string                `json:"kind"`
	State         domain.ConditionState `json:"state"`
	Used          *float64              `json:"used"`
	Limit         *float64              `json:"limit"`
	Percent       *float64              `json:"percent"`
	Threshold     *float64              `json:"threshold"`
	Unit          string                `json:"unit"`
	Resource      string                `json:"resource"`
	Scope         string                `json:"scope"`
	Source        string                `json:"source"`
	Message       string                `json:"message"`
	ObservedAt    string                `json:"observed_at"`
	StaleAfter    string                `json:"stale_after"`
	LastSuccessAt *string               `json:"last_success_at"`
}

// ConditionDeleteView is the WebSocket wire representation of a removed condition.
type ConditionDeleteView struct {
	MonitorID int64  `json:"monitor_id"`
	Kind      string `json:"kind"`
}

// marshalWireEvent converts a domain Event to frontend-compatible JSON.
func marshalWireEvent(event ports.Event) ([]byte, error) {
	return json.Marshal(wireEvent{
		Type:    event.Type,
		Payload: transformPayload(event.Type, event.Payload),
	})
}

func transformPayload(eventType string, payload any) any {
	switch eventType {
	case EventMonitorUpdate:
		switch v := payload.(type) {
		case *domain.Monitor:
			return toMonitorView(v, "pending")
		case MonitorView:
			return v // already resolved (e.g. from broadcastMonitorUpdate)
		case map[string]any:
			return monitorMapToView(v, "pending")
		}
	case EventMonitorList:
		if views, ok := payload.([]MonitorView); ok {
			return views
		}
		if monitors, ok := payload.([]*domain.Monitor); ok {
			views := make([]MonitorView, len(monitors))
			for i, m := range monitors {
				views[i] = toMonitorView(m, "pending")
			}
			return views
		}
	case EventHeartbeat:
		if hb, ok := payload.(*domain.Heartbeat); ok {
			return toHeartbeatView(hb)
		}
		if m, ok := payload.(map[string]any); ok {
			return heartbeatMapToView(m)
		}
	case EventConditionUpdate:
		switch v := payload.(type) {
		case *domain.MonitorCondition:
			return toConditionView(v, time.Now().UTC())
		case map[string]any:
			return conditionMapToView(v)
		}
	case EventConditionDelete:
		switch v := payload.(type) {
		case domain.ConditionDelete:
			return ConditionDeleteView{MonitorID: v.MonitorID, Kind: v.Kind}
		case *domain.ConditionDelete:
			return ConditionDeleteView{MonitorID: v.MonitorID, Kind: v.Kind}
		case map[string]any:
			id := extractInt64(v, "monitor_id")
			if id == 0 {
				id = extractInt64(v, "MonitorID")
			}
			return ConditionDeleteView{MonitorID: id, Kind: mapStr(v, "kind", "Kind")}
		}
	case EventStatusChange:
		return transformStatusChange(payload)
	}
	return payload
}

func toConditionView(condition *domain.MonitorCondition, now time.Time) MonitorConditionView {
	if condition == nil {
		return MonitorConditionView{}
	}
	view := MonitorConditionView{
		MonitorID:  condition.MonitorID,
		Kind:       condition.Kind,
		State:      condition.DisplayState(now),
		Used:       condition.Used,
		Limit:      condition.Limit,
		Percent:    condition.Percent,
		Threshold:  condition.Threshold,
		Unit:       condition.Unit,
		Resource:   condition.Resource,
		Scope:      condition.Scope,
		Source:     condition.Source,
		Message:    condition.Message,
		ObservedAt: formatTime(condition.ObservedAt),
		StaleAfter: formatTime(condition.StaleAfter),
	}
	if condition.LastSuccessAt != nil {
		lastSuccess := formatTime(*condition.LastSuccessAt)
		view.LastSuccessAt = &lastSuccess
	}
	return view
}

func conditionMapToView(values map[string]any) MonitorConditionView {
	view := MonitorConditionView{
		MonitorID:  extractInt64(values, "monitor_id"),
		Kind:       mapStr(values, "kind", "Kind"),
		State:      domain.ConditionState(mapStr(values, "state", "State")),
		Unit:       mapStr(values, "unit", "Unit"),
		Resource:   mapStr(values, "resource", "Resource"),
		Scope:      mapStr(values, "scope", "Scope"),
		Source:     mapStr(values, "source", "Source"),
		Message:    mapStr(values, "message", "Message"),
		ObservedAt: mapStr(values, "observed_at", "ObservedAt"),
		StaleAfter: mapStr(values, "stale_after", "StaleAfter"),
	}
	if view.MonitorID == 0 {
		view.MonitorID = extractInt64(values, "MonitorID")
	}
	view.Used = mapFloatPtr(values, "used", "Used")
	view.Limit = mapFloatPtr(values, "limit", "Limit")
	view.Percent = mapFloatPtr(values, "percent", "Percent")
	view.Threshold = mapFloatPtr(values, "threshold", "Threshold")
	if raw := mapStr(values, "last_success_at", "LastSuccessAt"); raw != "" {
		view.LastSuccessAt = &raw
	}
	if staleAt, err := time.Parse(time.RFC3339, view.StaleAfter); err == nil && !time.Now().UTC().Before(staleAt.UTC()) {
		view.State = domain.ConditionStateStale
	}
	return view
}

func mapFloatPtr(values map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok || raw == nil {
			continue
		}
		switch value := raw.(type) {
		case float64:
			return &value
		case float32:
			converted := float64(value)
			return &converted
		case int:
			converted := float64(value)
			return &converted
		case int64:
			converted := float64(value)
			return &converted
		}
	}
	return nil
}

func transformStatusChange(payload any) any {
	m, ok := payload.(map[string]any)
	if !ok {
		return payload
	}
	// monitor_id may arrive as int64 (direct Go map) or float64 (JSON-decoded).
	var monitorID int64
	switch v := m["monitor_id"].(type) {
	case int64:
		monitorID = v
	case int:
		monitorID = int64(v)
	case float64:
		monitorID = int64(v)
	}
	// new_status arrives as domain.Status (int) from Go code.
	var newStatus domain.Status
	switch v := m["new_status"].(type) {
	case domain.Status:
		newStatus = v
	case int:
		newStatus = domain.Status(v)
	case int64:
		newStatus = domain.Status(v)
	case float64:
		newStatus = domain.Status(v)
	}
	return map[string]any{
		"monitor_id": monitorID,
		"status":     statusToWire(newStatus),
	}
}

func toMonitorView(m *domain.Monitor, status string) MonitorView {
	if m == nil {
		return MonitorView{}
	}
	cfg := m.Config
	if cfg == nil {
		cfg = map[string]any{}
	}
	wireStatus := status
	if !m.Active {
		wireStatus = "paused"
	}
	return MonitorView{
		ID:                m.ID,
		Name:              m.Name,
		Owner:             m.Owner,
		InheritGroupOwner: m.InheritGroupOwner,
		EffectiveOwner:    m.Owner, // hub may overwrite when groups are available
		Type:              m.Type,
		Status:            wireStatus,
		Target:            m.Target(),
		Config:            cfg,
		Active:            m.Active,
		Interval:          m.Interval,
		Timeout:           m.Timeout,
		GroupID:           m.GroupID,
		Weight:            m.Weight,
		// Tags default to empty, never nil. Callers with a tag service (the hub)
		// overwrite this; callers without one still emit a valid [].
		Tags:      []MonitorTagView{},
		CreatedAt: formatTime(m.CreatedAt),
		UpdatedAt: formatTime(m.UpdatedAt),
	}
}

// monitorMapToView converts a generic map (from Redis JSON deserialization)
// into a MonitorView. This handles the case where the EventBus payload is
// a map[string]any instead of a typed struct.
// Domain.Monitor has no JSON tags so keys are capitalized (Name, Type, ID);
// MonitorView uses lowercase json tags (name, type, id). We try both.
func monitorMapToView(m map[string]any, status string) MonitorView {
	v := MonitorView{Status: status, Tags: []MonitorTagView{}}
	v.ID = extractInt64(m, "id")
	if v.ID == 0 {
		v.ID = extractInt64(m, "ID")
	}
	v.Name = mapStr(m, "name", "Name")
	v.Owner = mapStr(m, "owner", "Owner")
	v.EffectiveOwner = mapStr(m, "effective_owner", "EffectiveOwner")
	if v.EffectiveOwner == "" {
		v.EffectiveOwner = v.Owner
	}
	if b, ok := m["inherit_group_owner"].(bool); ok {
		v.InheritGroupOwner = b
	} else if b, ok := m["InheritGroupOwner"].(bool); ok {
		v.InheritGroupOwner = b
	}
	v.Type = mapStr(m, "type", "Type")
	v.Active, _ = m["active"].(bool)
	if !v.Active {
		v.Active, _ = m["Active"].(bool)
	}
	v.CreatedAt = mapStr(m, "created_at", "CreatedAt")
	v.UpdatedAt = mapStr(m, "updated_at", "UpdatedAt")
	v.Interval = int(extractInt64(m, "interval"))
	if v.Interval == 0 {
		v.Interval = int(extractInt64(m, "Interval"))
	}
	v.Timeout = extractFloat64(m, "timeout")
	if v.Timeout == 0 {
		v.Timeout = extractFloat64(m, "Timeout")
	}
	if cfg, ok := m["config"].(map[string]any); ok {
		v.Config = cfg
	} else if cfg, ok := m["Config"].(map[string]any); ok {
		v.Config = cfg
	} else {
		v.Config = map[string]any{}
	}
	if gid := extractInt64(m, "group_id"); gid != 0 {
		v.GroupID = &gid
	} else if gid := extractInt64(m, "GroupID"); gid != 0 {
		v.GroupID = &gid
	}
	v.Weight = int(extractInt64(m, "weight"))
	if v.Weight == 0 {
		v.Weight = int(extractInt64(m, "Weight"))
	}
	v.Target = monitorTarget(v.Type, v.Config)
	if !v.Active {
		v.Status = "paused"
	}
	return v
}

// mapStr returns the first non-empty string found under the given keys.
func mapStr(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// extractInt64 pulls an int64 value from a map[string]any, handling
// float64 (JSON decode) and int64/int variants.
func extractInt64(m map[string]any, key string) int64 {
	val, ok := m[key]
	if !ok {
		return 0
	}
	switch n := val.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

// extractFloat64 pulls a float64 value from a map[string]any.
func extractFloat64(m map[string]any, key string) float64 {
	val, ok := m[key]
	if !ok {
		return 0
	}
	switch n := val.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return 0
}

// heartbeatMapToView converts a generic map (e.g. Redis JSON) into HeartbeatView.
func heartbeatMapToView(m map[string]any) HeartbeatView {
	v := HeartbeatView{
		MonitorID: extractInt64(m, "monitor_id"),
		Ping:      int(extractInt64(m, "ping")),
		Msg:       mapStr(m, "msg", "Msg"),
	}
	if v.MonitorID == 0 {
		v.MonitorID = extractInt64(m, "MonitorID")
	}
	if v.Ping == 0 {
		v.Ping = int(extractInt64(m, "Ping"))
	}
	if t := mapStr(m, "time", "Time"); t != "" {
		v.Time = t
	}
	if s, ok := m["status"].(string); ok {
		v.Status = s
	} else if s, ok := m["Status"].(string); ok {
		v.Status = s
	} else {
		statusVal := extractInt64(m, "status")
		if statusVal == 0 {
			statusVal = extractInt64(m, "Status")
		}
		v.Status = statusToWire(domain.Status(statusVal))
	}
	return v
}

func toHeartbeatView(hb *domain.Heartbeat) HeartbeatView {
	if hb == nil {
		return HeartbeatView{}
	}
	status := statusToWire(hb.Status)
	return HeartbeatView{
		MonitorID: hb.MonitorID,
		Status:    status,
		Time:      formatTime(hb.Time),
		Ping:      hb.Ping,
		Msg:       hb.Msg,
	}
}

func statusToWire(s domain.Status) string {
	switch s {
	case domain.StatusUp:
		return "up"
	case domain.StatusDown:
		return "down"
	case domain.StatusPending:
		return "pending"
	case domain.StatusMaintenance:
		return "paused"
	default:
		return "pending"
	}
}

func monitorTarget(monitorType string, cfg map[string]any) string {
	keys := map[string][]string{
		"http":      {"url"},
		"websocket": {"url"},
		"tcp":       {"hostname", "host"},
		"ping":      {"hostname", "host"},
		"dns":       {"hostname", "host"},
		"grpc":      {"hostname"},
		"mqtt":      {"broker", "url", "hostname", "host"},
		"rabbitmq":  {"url", "connection_string", "dsn", "hostname", "host"},
		"snmp":      {"hostname", "host"},
		"database":  {"connectionString", "hostname"},
	}
	for _, key := range keys[monitorType] {
		if v, ok := cfg[key].(string); ok && v != "" {
			return safeMonitorTarget(monitorType, v)
		}
	}
	return ""
}

func safeMonitorTarget(monitorType, target string) string {
	if monitorType != "rabbitmq" {
		return target
	}
	u, err := url.Parse(target)
	if err != nil || u.User == nil {
		return target
	}
	u.User = nil
	return u.String()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// sendMonitorList pushes the initial monitor.list to one client, containing only
// the monitors that client is allowed to see.
//
// The old version filtered by MonitorFilter{UserID: client.UserID} — ownership.
// Under RBAC that is wrong in both directions: an admin must also see monitors
// owned by other users, and a granted non-admin must see monitors owned by the
// admin. The visible set from the AccessService is the only correct filter, and
// it is applied through MonitorFilter.RestrictToIDs so a client with zero grants
// receives an empty list rather than the whole install.
func (h *Hub) sendMonitorList(client *Client) {
	if h.monitorRepo == nil {
		return
	}
	ctx := clientCtx()
	all, visible := h.visibilityFor(ctx, client)
	if !all && len(visible) == 0 {
		// Nothing to show. Still send an explicit empty list so the dashboard
		// leaves its loading state instead of hanging on "connecting".
		h.sendMonitorViews(client, []MonitorView{})
		return
	}

	filter := ports.MonitorFilter{}
	if !all {
		filter.RestrictToIDs = true
		filter.MonitorIDs = make([]int64, 0, len(visible))
		for id := range visible {
			filter.MonitorIDs = append(filter.MonitorIDs, id)
		}
	}

	monitors, err := h.monitorRepo.List(ctx, filter)
	if err != nil {
		h.log.Error("ws: failed to load monitors for client", "user_id", client.UserID, "error", err)
		return
	}

	tagsByMonitor := map[int64][]services.MonitorTagDetail{}
	if h.tags != nil && len(monitors) > 0 {
		ids := make([]int64, len(monitors))
		for i, m := range monitors {
			ids[i] = m.ID
		}
		if fetched, tagErr := h.tags.TagsForMonitors(ctx, ids); tagErr == nil {
			tagsByMonitor = fetched
		} else {
			h.log.Warn("ws: batch tag lookup failed for monitor.list", "error", tagErr)
		}
	}

	// Resolve every monitor's status in ONE batched lookup, the same way
	// emitStatsUpdate does. This used to be a GetLatest per monitor per CONNECT:
	// at 1,000 monitors and 50 clients that was 50,000 serialized queries, and it
	// is why WebSocket connect p95 still failed its 1 s threshold after the
	// fan-out path itself had been fixed. Tags on the line below were already
	// batched; heartbeats were the straggler.
	statuses := h.latestStatuses(ctx, monitors)

	views := make([]MonitorView, len(monitors))
	for i, m := range monitors {
		status := "pending"
		if !m.Active {
			status = "paused"
		} else if st, ok := statuses[m.ID]; ok {
			status = statusToWire(st)
		}
		views[i] = toMonitorView(m, status)
		views[i].Tags = toWireTagViews(tagsByMonitor[m.ID])
	}
	h.sendMonitorViews(client, views)
}

func (h *Hub) sendMonitorViews(client *Client, views []MonitorView) {
	data, err := json.Marshal(wireEvent{Type: EventMonitorList, Payload: views})
	if err != nil {
		h.log.Error("ws: failed to marshal monitor.list", "error", err)
		return
	}
	select {
	case client.send <- data:
	default:
		// Counted as well as logged, so this shows up on /metrics alongside every
		// other dropped frame rather than only in a log nobody is grepping.
		h.log.Warn("ws: client send buffer full, dropped monitor.list", "client_id", client.ID)
		h.mu.RLock()
		m := h.metrics
		h.mu.RUnlock()
		if m != nil {
			m.IncWSFrameDropped()
		}
	}
}

func clientCtx() context.Context {
	// Background context for initial state push; no request cancellation needed.
	return context.Background()
}
