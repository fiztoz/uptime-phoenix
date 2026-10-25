package ws

import (
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Issue #89: monitor broadcasts reach every viewer of the monitor, so their
// config carries no credentials, push_token included.
func TestMarshalWireEvent_MonitorUpdateMasksSecrets(t *testing.T) {
	m := &domain.Monitor{
		ID: 7, Name: "db", Type: "http", Active: true,
		Config: map[string]any{
			"url":           "https://svc:url-pass-DO-NOT-LEAK@example.com/health",
			"auth_password": "basic-DO-NOT-LEAK",
			"push_token":    "push-DO-NOT-LEAK",
			"headers":       map[string]any{"Authorization": "Bearer DO-NOT-LEAK"},
		},
	}
	data, err := marshalWireEvent(ports.Event{Type: EventMonitorUpdate, Payload: m})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), "DO-NOT-LEAK") {
		t.Fatalf("monitor broadcast leaked a secret: %s", data)
	}
	if !strings.Contains(string(data), domain.SecretConfigPlaceholder) {
		t.Fatalf("expected placeholders in broadcast: %s", data)
	}
	if m.Config["auth_password"] != "basic-DO-NOT-LEAK" {
		t.Fatal("broadcast masking mutated the stored monitor")
	}
}

func TestMonitorMapToView_MasksSecrets(t *testing.T) {
	v := monitorMapToView(map[string]any{
		"id": float64(3), "type": "redis",
		"config": map[string]any{"url": "redis://:map-DO-NOT-LEAK@cache:6379", "password": "map-DO-NOT-LEAK"},
	}, "up")
	if v.Config["password"] != domain.SecretConfigPlaceholder {
		t.Errorf("password = %v; want placeholder", v.Config["password"])
	}
	if strings.Contains(v.Target, "DO-NOT-LEAK") || strings.Contains(v.Config["url"].(string), "DO-NOT-LEAK") {
		t.Errorf("url credential leaked: target=%q url=%v", v.Target, v.Config["url"])
	}
}

// The monitor card's target reaches every viewer of the monitor. For a
// database monitor Monitor.Target() used to be the raw DSN, and
// RedactURLPassword only understands scheme:// URLs, so Go MySQL DSNs and
// libpq keyword DSNs went out with their passwords.
func TestMonitorBroadcastTargetHidesDatabaseAndURLCredentials(t *testing.T) {
	cases := []struct {
		name, typ string
		cfg       map[string]any
		want      string
	}{
		{"mysql dsn", "database", map[string]any{"engine": "mysql", "connection_string": "monitor:pw-DO-NOT-LEAK@tcp(db:3306)/app?parseTime=true"}, "db:3306/app"},
		{"postgres keyword dsn", "database", map[string]any{"engine": "postgres", "connection_string": "host=pg port=5432 user=monitor password=pw-DO-NOT-LEAK dbname=app"}, "pg:5432/app"},
		{"url with password", "http", map[string]any{"url": "https://svc:pw-DO-NOT-LEAK@example.com/health"}, "https://example.com/health"},
	}
	for _, tc := range cases {
		m := &domain.Monitor{ID: 9, Name: tc.name, Type: tc.typ, Active: true, Config: tc.cfg}
		// Typed payload (in-process bus).
		data, err := marshalWireEvent(ports.Event{Type: EventMonitorUpdate, Payload: m})
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		if strings.Contains(string(data), "DO-NOT-LEAK") {
			t.Fatalf("%s: broadcast leaked a credential: %s", tc.name, data)
		}
		if v := toMonitorView(m, "up"); v.Target != tc.want {
			t.Errorf("%s: typed target = %q, want %q", tc.name, v.Target, tc.want)
		}
		// Map payload (Redis bus JSON round trip).
		mv := monitorMapToView(map[string]any{"id": float64(9), "type": tc.typ, "active": true, "config": tc.cfg}, "up")
		if mv.Target != tc.want {
			t.Errorf("%s: map target = %q, want %q", tc.name, mv.Target, tc.want)
		}
		if strings.Contains(mv.Target, "DO-NOT-LEAK") {
			t.Errorf("%s: map target leaked: %q", tc.name, mv.Target)
		}
	}
}
