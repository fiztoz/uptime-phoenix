package ws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestRegionalEventsRecheckAudienceAndStripPrivateFields(t *testing.T) {
	h := newHubRBACHarness(t)
	admin, member, stranger := h.addClient(h.adminID), h.addClient(h.memberID), h.addClient(h.strangerID)
	for _, kind := range []string{EventProbeStatus, EventProbeConfigStatus, EventMonitorProbeHeartbeat, EventMonitorProbeStatus, EventMonitorHealth, EventProbeCommandStatus} {
		t.Run(kind, func(t *testing.T) {
			fields := map[string]any{"id": "probe", "monitor_id": h.monitorGranted, "requester_id": h.memberID, "status": "up", "projection_version": "9223372036854775807", "revision": "9223372036854775807", "assignment_generation": "9223372036854775807", "observed_at": nil, "received_at": nil, "fresh_until": nil, "protected_payload": "private-secret", "note": "private-secret", "probe_counts": map[string]any{"assigned": 1, "secret": "private-secret"}, "regions": []any{map[string]any{"probe_id": "probe", "observed_at": nil, "endpoint": "private-secret"}}}
			h.hub.BroadcastRegional(ports.Event{Type: kind, Payload: fields})
			adminFrames := awaitFrames(admin, 5*time.Millisecond)
			memberFrames := awaitFrames(member, 5*time.Millisecond)
			if len(adminFrames) != 1 {
				t.Fatalf("admin missing event: %+v", adminFrames)
			}
			wantMember := 1
			if kind == EventProbeStatus || kind == EventProbeConfigStatus {
				wantMember = 0
			}
			if len(memberFrames) != wantMember || len(awaitFrames(stranger, 5*time.Millisecond)) != 0 {
				t.Fatalf("wrong audience: %+v", memberFrames)
			}
			raw, _ := json.Marshal(adminFrames[0])
			if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), "requester_id") {
				t.Fatalf("private wire field: %s", raw)
			}
			payload := adminFrames[0]["payload"].(map[string]any)
			if kind == EventMonitorHealth && payload["projection_version"] != "9223372036854775807" {
				t.Fatal("revision lost precision")
			}
			if kind == EventMonitorProbeStatus {
				if v, ok := payload["observed_at"]; !ok || v != nil {
					t.Fatal("missing evidence fabricated")
				}
			}
		})
	}
	if err := h.hub.access.RevokeMonitor(t.Context(), h.memberID, h.monitorGranted); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{EventMonitorProbeHeartbeat, EventMonitorProbeStatus, EventProbeCommandStatus} {
		h.hub.BroadcastRegional(ports.Event{Type: kind, Payload: map[string]any{"monitor_id": h.monitorGranted, "requester_id": h.memberID}})
	}
	if frames := awaitFrames(member, 10*time.Millisecond); len(frames) != 0 {
		t.Fatalf("revoked grant retained subscription: %+v", frames)
	}
}

func TestRegionalCommandEventRequesterScope(t *testing.T) {
	h := newHubRBACHarness(t)
	member := h.addClient(h.memberID)
	h.hub.BroadcastRegional(ports.Event{Type: EventProbeCommandStatus, Payload: map[string]any{"monitor_id": h.monitorGranted, "requester_id": h.strangerID, "command_id": "command", "status": "pending", "remote_confirmed": false}})
	if frames := awaitFrames(member, 10*time.Millisecond); len(frames) != 0 {
		t.Fatal("monitor visibility exposed another requester's receipt")
	}
}

func TestAccessChangePurgesConnectedClientScope(t *testing.T) {
	h := newHubRBACHarness(t)
	h.hub.access.SetEventBus(h.bus)
	member := h.addClient(h.memberID)
	if _, _, err := h.hub.access.VisibleMonitorIDs(t.Context(), h.memberID); err != nil {
		t.Fatal(err)
	}
	if err := h.hub.access.RevokeMonitor(t.Context(), h.memberID, h.monitorGranted); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	var invalidated bool
	for {
		select {
		case raw := <-member.Outbound():
			var frame map[string]any
			if err := json.Unmarshal(raw, &frame); err != nil {
				t.Fatal(err)
			}
			if frame["type"] == "access.changed" {
				invalidated = true
			}
			if frame["type"] == EventMonitorList {
				if !invalidated {
					t.Fatal("snapshot preceded cache invalidation")
				}
				if rows, ok := frame["payload"].([]any); !ok || len(rows) != 0 {
					t.Fatalf("snapshot retained inaccessible monitors: %s", raw)
				}
				return
			}
		case <-deadline:
			t.Fatal("access change did not refresh connected client")
		}
	}
}
