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

func TestGotifySender_Validate(t *testing.T) {
	s := GotifySender{}
	if err := s.Validate(map[string]any{"server_url": "https://gotify.example.com", "app_token": "tok"}); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	if err := s.Validate(map[string]any{"app_token": "tok"}); err == nil {
		t.Error("missing server_url should fail")
	}
	if err := s.Validate(map[string]any{"server_url": "https://gotify.example.com"}); err == nil {
		t.Error("missing app_token should fail")
	}
}

func TestGotifySender_Send_RequestShape(t *testing.T) {
	var method, path, token, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		token = r.URL.Query().Get("token")
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := GotifySender{}
	cfg := map[string]any{"server_url": srv.URL, "app_token": "AT-secret"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "timeout", CheckOutput: "curl: timed out"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
	if path != "/message" {
		t.Errorf("expected /message path, got %s", path)
	}
	if token != "AT-secret" {
		t.Errorf("expected token query param AT-secret, got %s", token)
	}
	if contentType != "application/json" {
		t.Errorf("expected json content-type, got %s", contentType)
	}
	title, _ := received["title"].(string)
	if !strings.Contains(title, "api") || !strings.Contains(title, "DOWN") {
		t.Errorf("expected monitor name + status in title, got %q", title)
	}
	message, _ := received["message"].(string)
	if !strings.Contains(message, "timeout") || !strings.Contains(message, "curl: timed out") {
		t.Errorf("expected message + check output, got %q", message)
	}
	if received["priority"].(float64) != 10 {
		t.Errorf("expected DOWN priority 10, got %v", received["priority"])
	}
}

func TestGotifySender_Send_PriorityMapping(t *testing.T) {
	cases := []struct {
		status   domain.Status
		priority float64
	}{
		{domain.StatusUp, 0},
		{domain.StatusDown, 10},
		{domain.StatusPending, 5},
		{domain.StatusMaintenance, 2},
	}
	for _, tc := range cases {
		var received map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&received)
			w.WriteHeader(http.StatusOK)
		}))
		s := GotifySender{}
		cfg := map[string]any{"server_url": srv.URL, "app_token": "tok"}
		alert := domain.AlertContext{MonitorName: "m", Status: tc.status}
		if err := s.Send(context.Background(), cfg, alert); err != nil {
			t.Fatalf("send failed for status %v: %v", tc.status, err)
		}
		if received["priority"].(float64) != tc.priority {
			t.Errorf("status %v: expected priority %v, got %v", tc.status, tc.priority, received["priority"])
		}
		srv.Close()
	}
}

func TestGotifySender_Send_CertificateExpiry(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := GotifySender{}
	cfg := map[string]any{"server_url": srv.URL, "app_token": "tok"}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if received["priority"].(float64) != 8 {
		t.Errorf("expected cert-expiry priority 8, got %v", received["priority"])
	}
	title, _ := received["title"].(string)
	if !strings.HasPrefix(title, "Phoenix:") {
		t.Errorf("expected Phoenix: prefix, got %q", title)
	}
}

func TestGotifySender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("invalid token"))
	}))
	defer srv.Close()

	s := GotifySender{}
	cfg := map[string]any{"server_url": srv.URL, "app_token": "bad"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid token") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestGotifySender_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	s := GotifySender{}
	cfg := map[string]any{"server_url": srv.URL, "app_token": "tok"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "gotify: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestGotifySender_RateLimitRetry(t *testing.T) {
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

	s := GotifySender{}
	cfg := map[string]any{"server_url": srv.URL, "app_token": "tok"}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}
