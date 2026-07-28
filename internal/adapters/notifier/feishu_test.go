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

func TestFeishuSender_Validate(t *testing.T) {
	s := FeishuSender{}
	if err := s.Validate(map[string]any{"webhook_url": "https://open.feishu.cn/hook/x"}); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing webhook_url should fail")
	}
}

func TestFeishuSender_Send_RequestShape(t *testing.T) {
	var method, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := FeishuSender{}
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
	if received["msg_type"] != "interactive" {
		t.Errorf("expected msg_type interactive, got %v", received["msg_type"])
	}
	card := received["card"].(map[string]any)
	header := card["header"].(map[string]any)
	if header["template"] != "red" {
		t.Errorf("expected DOWN template color red, got %v", header["template"])
	}
	titleContent := header["title"].(map[string]any)["content"].(string)
	if !strings.Contains(titleContent, "db") || !strings.Contains(titleContent, "DOWN") {
		t.Errorf("expected monitor name + status in header title, got %q", titleContent)
	}
	elements := card["elements"].([]any)
	el0 := elements[0].(map[string]any)
	body := el0["text"].(map[string]any)["content"].(string)
	if el0["text"].(map[string]any)["tag"] != "lark_md" {
		t.Errorf("expected lark_md tag for body text")
	}
	if !strings.Contains(body, "db:5432") || !strings.Contains(body, "conn refused") {
		t.Errorf("expected target + message in card body, got %q", body)
	}
}

func TestFeishuSender_Send_CertificateExpiry(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := FeishuSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	card := received["card"].(map[string]any)
	header := card["header"].(map[string]any)
	if header["template"] != "orange" {
		t.Errorf("expected orange template for cert expiry, got %v", header["template"])
	}
	elements := card["elements"].([]any)
	body := elements[0].(map[string]any)["text"].(map[string]any)["content"].(string)
	if !strings.Contains(body, "certificate_expiry") {
		t.Errorf("expected certificate_expiry event marker in body, got %q", body)
	}
}

func TestFeishuSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("sign verification failed"))
	}))
	defer srv.Close()

	s := FeishuSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "sign verification failed") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestFeishuSender_Send_TransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()

	s := FeishuSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "db", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "feishu: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestFeishuSender_RateLimitRetry(t *testing.T) {
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

	s := FeishuSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
}
