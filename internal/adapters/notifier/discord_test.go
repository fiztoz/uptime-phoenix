package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestDiscordSender_Validate(t *testing.T) {
	s := DiscordSender{}
	if err := s.Validate(map[string]any{"webhook_url": "https://discord.com/api/webhooks/1/abc"}); err != nil {
		t.Errorf("valid failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing webhook_url should fail")
	}
}

func TestDiscordSender_Send_DownSeverity(t *testing.T) {
	var received map[string]any
	var method, contentType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := DiscordSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{
		MonitorName:   "test-monitor",
		MonitorType:   "http",
		MonitorTarget: "https://example.com",
		Status:        domain.StatusDown,
		Message:       "down",
	}
	err := s.Send(context.Background(), cfg, alert)
	if err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
	if contentType != "application/json" {
		t.Errorf("expected json content-type")
	}
	embeds := received["embeds"].([]any)
	embed := embeds[0].(map[string]any)
	if embed["color"].(float64) != 0xFF0000 {
		t.Errorf("DOWN should have red color 0xFF0000, got %v", embed["color"])
	}

	// Verify Target field is included in the embed fields.
	fields := embed["fields"].([]any)
	var targetValue string
	for _, f := range fields {
		field := f.(map[string]any)
		if field["name"] == "Target" {
			targetValue = field["value"].(string)
		}
	}
	if targetValue != "https://example.com" {
		t.Errorf("expected Target field to be 'https://example.com', got %q", targetValue)
	}
}

func TestDiscordSender_RateLimitRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	s := DiscordSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	err := s.Send(context.Background(), cfg, alert)
	if err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}
