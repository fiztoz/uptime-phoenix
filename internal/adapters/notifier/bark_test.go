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

func TestBarkSender_Validate(t *testing.T) {
	s := BarkSender{}
	if err := s.Validate(map[string]any{"server_url": "https://api.day.app", "device_key": "abc123"}); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	if err := s.Validate(map[string]any{"device_key": "abc123"}); err == nil {
		t.Error("missing server_url should fail")
	}
	if err := s.Validate(map[string]any{"server_url": "https://api.day.app"}); err == nil {
		t.Error("missing device_key should fail")
	}
}

func TestBarkSender_Send_RequestShape(t *testing.T) {
	var method, path, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := BarkSender{}
	cfg := map[string]any{"server_url": srv.URL, "device_key": "dev-key-1"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "conn refused"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
	if path != "/dev-key-1" {
		t.Errorf("expected path /dev-key-1 (device key in URL), got %s", path)
	}
	if contentType != "application/json" {
		t.Errorf("expected json content-type, got %s", contentType)
	}
	if received["group"] != "Phoenix" {
		t.Errorf("expected group Phoenix, got %v", received["group"])
	}
	title, _ := received["title"].(string)
	if !strings.Contains(title, "🔴") || !strings.Contains(title, "api") {
		t.Errorf("expected DOWN emoji + monitor name in title, got %q", title)
	}
	body, _ := received["body"].(string)
	if !strings.Contains(body, "conn refused") || !strings.Contains(body, "DOWN") {
		t.Errorf("expected status + message in body, got %q", body)
	}
}

func TestBarkSender_Send_CertificateExpiry(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := BarkSender{}
	cfg := map[string]any{"server_url": srv.URL, "device_key": "dev-key-1"}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
		CertThreshold:     14,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	title, _ := received["title"].(string)
	if !strings.HasPrefix(title, "📜") {
		t.Errorf("expected certificate emoji prefix, got %q", title)
	}
	body, _ := received["body"].(string)
	if !strings.Contains(body, "7 day") {
		t.Errorf("expected days-remaining in cert body, got %q", body)
	}
}

func TestBarkSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid device key"))
	}))
	defer srv.Close()

	s := BarkSender{}
	cfg := map[string]any{"server_url": srv.URL, "device_key": "bad-key"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid device key") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestBarkSender_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // closed before use: connection refused

	s := BarkSender{}
	cfg := map[string]any{"server_url": srv.URL, "device_key": "dev-key-1"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "bark: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestBarkSender_RateLimitRetry(t *testing.T) {
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

	s := BarkSender{}
	cfg := map[string]any{"server_url": srv.URL, "device_key": "dev-key-1"}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}
