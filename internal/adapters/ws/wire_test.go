package ws

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestMarshalWireEvent_MonitorUpdate(t *testing.T) {
	m := &domain.Monitor{
		ID:        1,
		Name:      "Example",
		Owner:     "Platform on-call",
		Type:      "http",
		Active:    true,
		Interval:  10,
		Timeout:   30,
		Config:    map[string]any{"url": "https://example.com"},
		CreatedAt: time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 24, 0, 0, 0, 0, time.UTC),
	}
	data, err := marshalWireEvent(ports.Event{Type: EventMonitorUpdate, Payload: m})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["type"] != EventMonitorUpdate {
		t.Errorf("type = %v, want %q", got["type"], EventMonitorUpdate)
	}
	payload, ok := got["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", got["payload"])
	}
	if payload["id"] != float64(1) {
		t.Errorf("id = %v, want 1", payload["id"])
	}
	if payload["name"] != "Example" {
		t.Errorf("name = %v", payload["name"])
	}
	if payload["owner"] != "Platform on-call" {
		t.Errorf("owner = %v", payload["owner"])
	}
	if payload["status"] != "pending" {
		t.Errorf("status = %v, want pending", payload["status"])
	}
	if payload["target"] != "https://example.com" {
		t.Errorf("target = %v", payload["target"])
	}
	if payload["interval"] != float64(10) {
		t.Errorf("interval = %v, want 10", payload["interval"])
	}
	if payload["timeout"] != float64(30) {
		t.Errorf("timeout = %v, want 30", payload["timeout"])
	}
}

func TestMonitorTarget_RabbitMQRedactsUserinfo(t *testing.T) {
	target := monitorTarget("rabbitmq", map[string]any{"url": "amqp://monitor:secret@rabbitmq.internal:5672/%2F"})
	if target != "amqp://rabbitmq.internal:5672/%2F" {
		t.Fatalf("target = %q, want redacted URL", target)
	}
}

func TestMarshalWireEvent_MonitorList(t *testing.T) {
	monitors := []*domain.Monitor{
		{ID: 1, Name: "A", Type: "http", Config: map[string]any{"url": "https://a.test"}},
		{ID: 2, Name: "B", Type: "ping", Config: map[string]any{"hostname": "1.1.1.1"}},
	}
	data, err := marshalWireEvent(ports.Event{Type: EventMonitorList, Payload: monitors})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	payload, ok := got["payload"].([]any)
	if !ok {
		t.Fatalf("payload type = %T", got["payload"])
	}
	if len(payload) != 2 {
		t.Errorf("len(payload) = %d, want 2", len(payload))
	}
}
