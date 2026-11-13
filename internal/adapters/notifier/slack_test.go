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

func TestSlackSender_Validate(t *testing.T) {
	s := SlackSender{}
	if err := s.Validate(map[string]any{"webhook_url": "https://hooks.slack.com/services/xxx"}); err != nil {
		t.Errorf("valid failed: %v", err)
	}
	if err := s.Validate(map[string]any{}); err == nil {
		t.Error("missing url should fail")
	}
}

func TestSlackSender_Send_DownSeverity(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := SlackSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "fail"}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send err: %v", err)
	}
	blocks := received["blocks"].([]any)
	// check first block header contains DOWN emoji or text
	header := blocks[0].(map[string]any)
	txt := header["text"].(map[string]any)["text"].(string)
	if !strings.Contains(txt, ":x:") && !strings.Contains(txt, "DOWN") {
		t.Errorf("expected :x: or DOWN in header, got %s", txt)
	}
}

func TestSlackSender_RateLimitRetry(t *testing.T) {
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

	s := SlackSender{}
	cfg := map[string]any{"webhook_url": srv.URL}
	alert := domain.AlertContext{Status: domain.StatusDown}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	if attempts != 2 {
		t.Errorf("expected retry, got %d attempts", attempts)
	}
}

func TestSlackSender_Send_EmptyTargetOmitted(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	alert := domain.AlertContext{MonitorName: "api", MonitorType: "http", Status: domain.StatusDown, Message: "fail"}
	if err := (SlackSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	blocks := received["blocks"].([]any)
	// Header is block 0; the section with the body text is block 1.
	section := blocks[1].(map[string]any)
	text := section["text"].(map[string]any)["text"].(string)
	if strings.Contains(text, "Target:") {
		t.Errorf("empty target must not render a Target line, got %q", text)
	}
}

// M7 scope text: in `both` mode a channel gets a regional page and a hub
// aggregate page for the same monitor. The Slack header block is what people
// read first, so it must tell the two apart.
func TestSlackSender_Send_AggregateHeaderNamesScope(t *testing.T) {
	headerFor := func(alert domain.AlertContext) string {
		t.Helper()
		var received map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&received)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		if err := (SlackSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
			t.Fatalf("send err: %v", err)
		}
		return received["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
	}
	regional := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "fail",
		AlertScope: domain.AlertScopeMonitor, DeliveryScope: domain.IncidentScopeRegional, ProbeID: "p1"}
	aggregate := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown, Message: "fail",
		AlertScope: domain.AlertScopeMonitor, DeliveryScope: domain.IncidentScopeAggregate, SourceAlertID: "aggregate-1"}
	reg, agg := headerFor(regional), headerFor(aggregate)
	if reg == agg {
		t.Fatalf("regional and aggregate Slack headers must differ, both %q", reg)
	}
	if !strings.HasSuffix(agg, "(aggregate)") {
		t.Fatalf("aggregate Slack header = %q, want the (aggregate) suffix", agg)
	}
	if strings.Contains(reg, "(aggregate)") {
		t.Fatalf("regional Slack header = %q must not claim aggregate scope", reg)
	}
}
