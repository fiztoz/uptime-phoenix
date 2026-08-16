package ws

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMonitorIDForEvent_ConditionDeleteShapes(t *testing.T) {
	t.Parallel()

	if id, ok := monitorIDForEvent(ports.Event{Payload: domain.ConditionDelete{MonitorID: 42, Kind: "storage"}}); !ok || id != 42 {
		t.Fatalf("value delete id=%d ok=%v", id, ok)
	}
	payload := &domain.ConditionDelete{MonitorID: 7, Kind: "session_pool"}
	if id, ok := monitorIDForEvent(ports.Event{Payload: payload}); !ok || id != 7 {
		t.Fatalf("pointer delete id=%d ok=%v", id, ok)
	}
	var none *domain.ConditionDelete
	if _, ok := monitorIDForEvent(ports.Event{Payload: none}); ok {
		t.Fatal("nil pointer must not resolve")
	}
	if id, ok := monitorIDForEvent(ports.Event{Payload: map[string]any{"monitor_id": float64(9), "kind": "storage"}}); !ok || id != 9 {
		t.Fatalf("map delete id=%d ok=%v", id, ok)
	}
	if _, ok := monitorIDForEvent(ports.Event{Payload: domain.ConditionDelete{}}); ok {
		t.Fatal("zero id must not resolve")
	}
}

func TestHub_MemoryBusDeliversTypedConditionDelete(t *testing.T) {
	h := newHubRBACHarness(t)
	member := h.addClient(h.memberID)
	stranger := h.addClient(h.strangerID)

	if err := h.bus.Publish(context.Background(), ports.Event{
		Type:    EventConditionDelete,
		Payload: domain.ConditionDelete{MonitorID: h.monitorGranted, Kind: domain.MonitorConditionStorage},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	memberFrames := awaitFrames(member, 250*time.Millisecond)
	if got := conditionDeleteFramesFor(memberFrames, h.monitorGranted, domain.MonitorConditionStorage); len(got) != 1 {
		t.Fatalf("granted client frames=%v", memberFrames)
	}
	payload := conditionDeleteFramesFor(memberFrames, h.monitorGranted, domain.MonitorConditionStorage)[0]["payload"].(map[string]any)
	if payload["monitor_id"] != float64(h.monitorGranted) || payload["kind"] != domain.MonitorConditionStorage {
		t.Fatalf("wire payload=%v", payload)
	}

	strangerFrames := awaitFrames(stranger, 250*time.Millisecond)
	if got := conditionDeleteFramesFor(strangerFrames, h.monitorGranted, domain.MonitorConditionStorage); len(got) != 0 {
		t.Fatalf("unauthorized client received %v", strangerFrames)
	}
}

func TestHub_MemoryBusAcceptsMapConditionDelete(t *testing.T) {
	h := newHubRBACHarness(t)
	member := h.addClient(h.memberID)
	if err := h.bus.Publish(context.Background(), ports.Event{
		Type:    EventConditionDelete,
		Payload: map[string]any{"monitor_id": float64(h.monitorGranted), "kind": domain.MonitorConditionSessionPool},
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	frames := awaitFrames(member, 250*time.Millisecond)
	if got := conditionDeleteFramesFor(frames, h.monitorGranted, domain.MonitorConditionSessionPool); len(got) != 1 {
		t.Fatalf("map-shaped delete frames=%v", frames)
	}
}

func conditionDeleteFramesFor(frames []map[string]any, monitorID int64, kind string) []map[string]any {
	var out []map[string]any
	for _, frame := range frames {
		if frame["type"] != EventConditionDelete {
			continue
		}
		payload, ok := frame["payload"].(map[string]any)
		if !ok {
			continue
		}
		id, _ := payload["monitor_id"].(float64)
		if int64(id) == monitorID && payload["kind"] == kind {
			out = append(out, frame)
		}
	}
	return out
}
