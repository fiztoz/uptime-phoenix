package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// redisRoundTrip mirrors eventbus.RedisBus: Publish JSON-encodes the whole
// ports.Event and the listener decodes it back into a ports.Event, so every
// typed payload arrives as map[string]any.
func redisRoundTrip(t *testing.T, ev ports.Event) ports.Event {
	t.Helper()
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var round ports.Event
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	return round
}

// A heartbeat published with an overall projection is a HeartbeatPublication.
// Over the Redis bus it becomes {"Heartbeat":{"MonitorID":7,...},...}, so the
// monitor id sits one level down. broadcast drops any event whose monitor id it
// cannot resolve, which would silently drop every projected heartbeat in split
// (REDIS_URL) mode while the in-process bus kept working.
func TestMonitorIDForEvent_RedisShapedHeartbeatPublication(t *testing.T) {
	overall := domain.StatusUp
	pub := domain.HeartbeatPublication{
		Heartbeat: &domain.Heartbeat{
			MonitorID: 7,
			Status:    domain.StatusUp,
			Time:      time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC),
		},
		Overall:           &overall,
		ProjectionVersion: 3,
	}
	for name, ev := range map[string]ports.Event{
		"typed":       {Type: EventHeartbeat, Payload: pub},
		"typed-ptr":   {Type: EventHeartbeat, Payload: &pub},
		"redis-shape": redisRoundTrip(t, ports.Event{Type: EventHeartbeat, Payload: pub}),
	} {
		t.Run(name, func(t *testing.T) {
			id, ok := monitorIDForEvent(ev)
			if !ok || id != 7 {
				t.Fatalf("monitorIDForEvent = (%d, %v), want (7, true)", id, ok)
			}
		})
	}
}

// A plain heartbeat (no overall projection) already resolves after a round
// trip; keep it pinned so a fix for the nested shape cannot regress it.
func TestMonitorIDForEvent_RedisShapedPlainHeartbeat(t *testing.T) {
	ev := redisRoundTrip(t, ports.Event{Type: EventHeartbeat, Payload: &domain.Heartbeat{MonitorID: 9, Status: domain.StatusDown}})
	if id, ok := monitorIDForEvent(ev); !ok || id != 9 {
		t.Fatalf("monitorIDForEvent = (%d, %v), want (9, true)", id, ok)
	}
}
