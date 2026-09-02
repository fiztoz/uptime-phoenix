package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestTeamsSender_Validate(t *testing.T) {
	s := TeamsSender{}
	if err := s.Validate(map[string]any{"webhook_url": "https://outlook.office.com/webhook/x"}); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing webhook_url should fail")
	}
}

func TestTeamsSender_Send_RequestShape(t *testing.T) {
	var method, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := TeamsSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
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
		t.Errorf("expected POST, got %s", method)
	}
	if contentType != "application/json" {
		t.Errorf("expected json content-type, got %s", contentType)
	}
	if received["@type"] != "MessageCard" {
		t.Errorf("expected MessageCard type, got %v", received["@type"])
	}
	if received["themeColor"] != "FF0000" {
		t.Errorf("expected DOWN theme color FF0000, got %v", received["themeColor"])
	}
	title, _ := received["title"].(string)
	if !strings.Contains(title, "db") || !strings.Contains(title, "DOWN") {
		t.Errorf("expected monitor + status in title, got %q", title)
	}
	text, _ := received["text"].(string)
	if !strings.Contains(text, "conn refused") || !strings.Contains(text, "db:5432") {
		t.Errorf("expected message + target in text, got %q", text)
	}
	sections := received["sections"].([]any)
	facts := sections[0].(map[string]any)["facts"].([]any)
	var sawStatus, sawMonitor bool
	for _, f := range facts {
		fact := f.(map[string]any)
		if fact["name"] == "Status" && fact["value"] == "DOWN" {
			sawStatus = true
		}
		if fact["name"] == "Monitor" && fact["value"] == "db" {
			sawMonitor = true
		}
	}
	if !sawStatus || !sawMonitor {
		t.Errorf("expected Status/Monitor facts, got %+v", facts)
	}
}

func TestTeamsSender_Send_CertificateExpiry(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := TeamsSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if received["themeColor"] != "FFA500" {
		t.Errorf("expected cert-expiry theme color FFA500, got %v", received["themeColor"])
	}
	sections := received["sections"].([]any)
	facts := sections[0].(map[string]any)["facts"].([]any)
	var sawEvent bool
	for _, f := range facts {
		fact := f.(map[string]any)
		if fact["name"] == "Event" && fact["value"] == "certificate_expiry" {
			sawEvent = true
		}
	}
	if !sawEvent {
		t.Errorf("expected Event=certificate_expiry fact, got %+v", facts)
	}
}

func TestTeamsSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream error"))
	}))
	defer srv.Close()

	s := TeamsSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "upstream error") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestTeamsSender_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	s := TeamsSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "teams: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestTeamsSender_RateLimitRetry(t *testing.T) {
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

	s := TeamsSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}

func TestTeamsSender_Send_EmptyTargetOmitted(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	alert := domain.AlertContext{MonitorName: "db", MonitorType: "tcp", Status: domain.StatusDown, Message: "conn refused"}
	if err := (TeamsSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	text, _ := received["text"].(string)
	if strings.Contains(text, "Target:") {
		t.Errorf("empty target must not render a Target line, got %q", text)
	}
}
