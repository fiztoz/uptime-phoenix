package notifier

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestWebhookSender_Validate(t *testing.T) {
	s := WebhookSender{}
	if err := s.Validate(map[string]any{"url": "https://example.com/hook"}); err != nil {
		t.Errorf("valid failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing url should fail")
	}
}

func TestWebhookSender_Send_DownSeverity(t *testing.T) {
	var received map[string]any
	var method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := WebhookSender{}
	cfg := map[string]any{"url": srv.URL}
	alert := domain.AlertContext{
		MonitorName:   "db",
		MonitorType:   "tcp",
		MonitorTarget: "db:5432",
		Status:        domain.StatusDown,
		Message:       "conn refused",
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if method != "POST" {
		t.Errorf("expected POST got %s", method)
	}
	if received["severity"] != "DOWN" {
		t.Errorf("expected severity DOWN, got %v", received["severity"])
	}
	mon := received["monitor"].(map[string]any)
	if mon["name"] != "db" {
		t.Error("monitor name mismatch")
	}
}

func TestWebhookSender_RateLimitRetry(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := WebhookSender{}
	cfg := map[string]any{"url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestWebhookSender_Template(t *testing.T) {
	var bodyStr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodyStr = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := WebhookSender{}
	cfg := map[string]any{
		"url":           srv.URL,
		"body_template": `{"custom":"{{.MonitorName}} is {{.Status}}"}`,
	}
	alert := domain.AlertContext{MonitorName: "tpl", Status: domain.StatusUp}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if !strings.Contains(bodyStr, "tpl is UP") {
		t.Errorf("template not rendered, got %s", bodyStr)
	}
}
