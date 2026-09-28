package ws

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// BroadcastRegional maps only explicit browser fields, then rechecks the current
// AccessService for every recipient. It deliberately bypasses the hub's legacy
// visibility TTL so revoked grants cannot retain a regional subscription.
func (h *Hub) BroadcastRegional(event ports.Event) {
	raw, err := json.Marshal(event.Payload)
	if err != nil {
		return
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&fields) != nil {
		return
	}
	payload := regionalWirePayload(event.Type, fields)
	if payload == nil {
		return
	}
	id := regionalNumber(fields["monitor_id"])
	requester := regionalNumber(fields["requester_id"])
	adminOnly := event.Type == EventProbeStatus || event.Type == EventProbeConfigStatus
	if !adminOnly && id <= 0 {
		return
	}
	data, err := json.Marshal(wireEvent{Type: event.Type, Payload: payload})
	if err != nil || h.access == nil {
		return
	}
	ctx := context.Background()
	for _, client := range h.snapshotClients() {
		if client.UserID <= 0 {
			continue
		}
		if adminOnly || event.Type == EventProbeCommandStatus {
			admin, err := h.access.IsAdmin(ctx, client.UserID)
			if err != nil || (!admin && (adminOnly || requester != client.UserID)) {
				continue
			}
		}
		if !adminOnly {
			allowed, err := h.access.CanViewMonitor(ctx, client.UserID, id)
			if err != nil || !allowed {
				continue
			}
		}
		h.send(client, data)
	}
}

func regionalNumber(value any) int64 {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	id, err := number.Int64()
	if err != nil {
		return 0
	}
	return id
}

func selectRegionalFields(source map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		out[key] = source[key]
	}
	return out
}

func regionalWirePayload(kind string, fields map[string]any) map[string]any {
	switch kind {
	case EventProbeStatus:
		return selectRegionalFields(fields, "id", "key", "name", "location", "kind", "enabled", "enrollment_state", "connection_status", "execution_status", "last_seen_at", "revision")
	case EventProbeConfigStatus:
		return selectRegionalFields(fields, "probe_id", "revision", "status", "errors")
	case EventProbeCommandStatus:
		return selectRegionalFields(fields, "command_id", "status", "remote_confirmed")
	case EventMonitorProbeHeartbeat:
		return selectRegionalFields(fields, "id", "monitor_id", "probe_id", "status", "ping", "message", "time", "important", "received_at", "assignment_generation", "config_revision")
	case EventMonitorProbeStatus:
		return selectRegionalFields(fields, "monitor_id", "probe_id", "status", "connection_status", "observed_at", "received_at", "fresh_until", "reason")
	case EventMonitorHealth:
		out := selectRegionalFields(fields, "monitor_id", "status", "health_policy", "projection_version", "as_of", "uptime_percent", "coverage_percent", "known_seconds", "unknown_seconds", "maintenance_seconds")
		counts, ok := fields["probe_counts"].(map[string]any)
		if !ok {
			return nil
		}
		out["probe_counts"] = selectRegionalFields(counts, "assigned", "up", "down", "pending", "unknown", "maintenance", "paused")
		rows, ok := fields["regions"].([]any)
		if !ok {
			return nil
		}
		regions := make([]any, 0, len(rows))
		for _, row := range rows {
			region, ok := row.(map[string]any)
			if !ok {
				return nil
			}
			regions = append(regions, selectRegionalFields(region, "probe_id", "name", "location", "status", "connection_status", "observed_at", "received_at", "fresh_until", "config_sync_status", "reason"))
		}
		out["regions"] = regions
		return out
	}
	return nil
}
