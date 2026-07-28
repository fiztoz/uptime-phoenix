package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// lineTestTransport intercepts requests made through http.DefaultClient,
// records the URL the sender actually addressed (LineSender hardcodes the
// LINE API endpoint, so there is nothing to inject at the config level),
// and redirects the connection to a local httptest server so the request
// method/headers/body can be inspected like any other adapter.
type lineTestTransport struct {
	target *url.URL
	fail   error

	mu          sync.Mutex
	capturedURL string
	requests    int
}

func (t *lineTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.capturedURL = req.URL.String()
	t.requests++
	t.mu.Unlock()

	if t.fail != nil {
		return nil, t.fail
	}

	redirected := req.Clone(req.Context())
	redirected.URL = &url.URL{
		Scheme:   t.target.Scheme,
		Host:     t.target.Host,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
	}
	redirected.Host = t.target.Host
	return http.DefaultTransport.RoundTrip(redirected)
}

func (t *lineTestTransport) URL() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.capturedURL
}

func (t *lineTestTransport) Requests() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.requests
}

// installLineTransport swaps http.DefaultClient's transport for the
// duration of the test (LineSender always calls http.DefaultClient.Do).
// Notifier package tests never run in parallel, so this is safe.
func installLineTransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	orig := http.DefaultClient.Transport
	http.DefaultClient.Transport = rt
	t.Cleanup(func() { http.DefaultClient.Transport = orig })
}

const lineExpectedURL = "https://api.line.me/v2/bot/message/push"

func TestLineSender_Validate(t *testing.T) {
	s := LineSender{}
	if err := s.Validate(map[string]any{"channel_access_token": "tok", "user_id": "U123"}); err != nil {
		t.Errorf("valid (user_id) config failed: %v", err)
	}
	if err := s.Validate(map[string]any{"channel_access_token": "tok", "group_id": "G123"}); err != nil {
		t.Errorf("valid (group_id) config failed: %v", err)
	}
	if err := s.Validate(map[string]any{"channel_access_token": "tok", "room_id": "R123"}); err != nil {
		t.Errorf("valid (room_id) config failed: %v", err)
	}
	if err := s.Validate(map[string]any{"user_id": "U123"}); err == nil {
		t.Error("missing channel_access_token should fail")
	}
	if err := s.Validate(map[string]any{"channel_access_token": "tok"}); err == nil {
		t.Error("missing target (user/group/room id) should fail")
	}
}

func TestLineSender_Send_RequestShape(t *testing.T) {
	var method, auth, contentType string
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		auth = r.Header.Get("Authorization")
		contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	rt := &lineTestTransport{target: target}
	installLineTransport(t, rt)

	s := LineSender{}
	cfg := map[string]any{"channel_access_token": "chan-tok", "user_id": "U123"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "conn refused"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	if rt.URL() != lineExpectedURL {
		t.Errorf("expected sender to address %s, got %s", lineExpectedURL, rt.URL())
	}
	if method != "POST" {
		t.Errorf("expected POST, got %s", method)
	}
	if auth != "Bearer chan-tok" {
		t.Errorf("expected Bearer channel token, got %q", auth)
	}
	if contentType != "application/json" {
		t.Errorf("expected json content-type, got %s", contentType)
	}
	if received["to"] != "U123" {
		t.Errorf("expected to=U123, got %v", received["to"])
	}
	messages := received["messages"].([]any)
	msg := messages[0].(map[string]any)
	if msg["type"] != "text" {
		t.Errorf("expected text message type, got %v", msg["type"])
	}
	text, _ := msg["text"].(string)
	if !strings.Contains(text, "api") || !strings.Contains(text, "DOWN") || !strings.Contains(text, "conn refused") {
		t.Errorf("expected monitor/status/message in text, got %q", text)
	}
}

func TestLineSender_Send_CertificateExpiry(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target, _ := url.Parse(srv.URL)
	installLineTransport(t, &lineTestTransport{target: target})

	s := LineSender{}
	cfg := map[string]any{"channel_access_token": "chan-tok", "user_id": "U123"}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	messages := received["messages"].([]any)
	text, _ := messages[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(text, "📜") {
		t.Errorf("expected certificate emoji prefix, got %q", text)
	}
}

func TestLineSender_Send_NonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("invalid reply token"))
	}))
	defer srv.Close()

	target, _ := url.Parse(srv.URL)
	installLineTransport(t, &lineTestTransport{target: target})

	s := LineSender{}
	cfg := map[string]any{"channel_access_token": "chan-tok", "user_id": "U123"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error on non-200 response")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid reply token") {
		t.Errorf("expected error to surface status + body, got %v", err)
	}
}

func TestLineSender_Send_TransportError(t *testing.T) {
	installLineTransport(t, &lineTestTransport{fail: errors.New("network unreachable")})

	s := LineSender{}
	cfg := map[string]any{"channel_access_token": "chan-tok", "user_id": "U123"}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected transport error")
	}
	if !strings.Contains(err.Error(), "line: sending message") {
		t.Errorf("expected wrapped transport error, got %v", err)
	}
}

func TestLineSender_RateLimitRetry(t *testing.T) {
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

	target, _ := url.Parse(srv.URL)
	rt := &lineTestTransport{target: target}
	installLineTransport(t, rt)

	s := LineSender{}
	cfg := map[string]any{"channel_access_token": "chan-tok", "user_id": "U123"}
	alert := domain.AlertContext{Status: domain.StatusDown, MonitorName: "rl-test"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("retry send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts (retry), got %d", attempts)
	}
	if rt.Requests() != 2 {
		t.Errorf("expected transport to see 2 requests, got %d", rt.Requests())
	}
}
