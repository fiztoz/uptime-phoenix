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

func TestMattermostSender_Validate(t *testing.T) {
	s := MattermostSender{}
	if err := s.Validate(map[string]any{"webhook_url": "https://mm.example.com/hooks/x"}); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing webhook_url should fail")
	}
}

func TestMattermostSender_Send_RequestShape(t *testing.T) {
	var method, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := MattermostSender{}
	cfg := map[string]any{"webhook_url": srv.URL, "channel": "alerts", "username": "PhoenixBot"}
	alert := domain.AlertContext{
		MonitorName:   "db",
		MonitorType:   "tcp",
		MonitorTarget: "db:5432",
		Status:        domain.StatusDown,
		Message:       "conn refused",
		CheckOutput:   "dial tcp: refused",
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
	if received["username"] != "PhoenixBot" {
		t.Errorf("expected configured username, got %v", received["username"])
	}
	if received["channel"] != "alerts" {
		t.Errorf("expected configured channel, got %v", received["channel"])
	}
	attachments := received["attachments"].([]any)
	att := attachments[0].(map[string]any)
	if att["color"] != "#FF0000" {
		t.Errorf("expected DOWN color #FF0000, got %v", att["color"])
	}
	text, _ := att["text"].(string)
	if !strings.Contains(text, "conn refused") || !strings.Contains(text, "dial tcp: refused") {
		t.Errorf("expected message + check output in attachment text, got %q", text)
	}
	fields := att["fields"].([]any)
	var sawType, sawTarget bool
	for _, f := range fields {
		field := f.(map[string]any)
		if field["title"] == "Type" && field["value"] == "tcp" {
			sawType = true
		}
		if field["title"] == "Target" && field["value"] == "db:5432" {
			sawTarget = true
		}
	}
	if !sawType || !sawTarget {
		t.Errorf("expected Type/Target fields in attachment, got %+v", fields)
	}
}

func TestMattermostSender_Send_OmitsChannelWhenNotConfigured(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := MattermostSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusUp}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if _, ok := received["channel"]; ok {
		t.Errorf("expected no channel key when not configured, got %v", received["channel"])
	}
	if received["username"] != "Phoenix" {
		t.Errorf("expected default username Phoenix, got %v", received["username"])
	}
}

func TestMattermostSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unknown webhook"))
	}))
	defer srv.Close()

	s := MattermostSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "unknown webhook") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestMattermostSender_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	s := MattermostSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "mattermost: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestMattermostSender_RateLimitRetry(t *testing.T) {
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

	s := MattermostSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}
