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

func TestDiscordSender_Send_EmptyTargetOmitted(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	// Monitor alert whose target was blanked by include_target=false: the
	// default embed's Target field must disappear, never an empty value.
	alert := domain.AlertContext{
		AlertScope:    domain.AlertScopeMonitor,
		MonitorName:   "test-monitor",
		MonitorType:   "http",
		MonitorTarget: "",
		Status:        domain.StatusDown,
		Message:       "down",
	}
	if err := (DiscordSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	embed := received["embeds"].([]any)[0].(map[string]any)
	fields, ok := embed["fields"].([]any)
	if !ok {
		t.Fatal("expected embed fields array")
	}
	for _, f := range fields {
		field := f.(map[string]any)
		if field["name"] == "Target" {
			t.Fatalf("Target field should be omitted when the target is empty, got %#v", field)
		}
	}
}

func TestDiscordSender_Send_CustomTemplate(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	alert := domain.AlertContext{
		MonitorName: "payments", Status: domain.StatusDown, Message: "connection refused",
		TemplateTitle: "Incident: {{ monitor.name }}",
		TemplateBody:  "{{ monitor.name }} changed to {{ status }} — {{ message }}",
	}
	if err := (DiscordSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send custom template: %v", err)
	}
	embed := received["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "Incident: payments" {
		t.Fatalf("custom title = %v", embed["title"])
	}
	if embed["description"] != "payments changed to DOWN — connection refused" {
		t.Fatalf("custom description = %v", embed["description"])
	}
}

func TestDiscordSender_Send_StructuredGroupTemplate(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	config := domain.DefaultDiscordTemplateConfig()
	config.TitleURLTemplate = "{{ alert.target }}"
	config.FooterTemplate = "Phoenix • {{ alert.scope }}"
	config.Colors.Down = "#123456"
	config.Fields = []domain.DiscordEmbedFieldTemplate{
		{NameTemplate: "Target", ValueTemplate: "{{ alert.target }}"},
		{NameTemplate: "Condition", ValueTemplate: "{{ group.condition }}", Inline: true},
		{NameTemplate: "Threshold", ValueTemplate: "{{ group.threshold_display }}", Inline: true},
	}
	alert := domain.AlertContext{
		AlertScope: domain.AlertScopeGroup, GroupID: 7, GroupName: "Platform",
		MonitorName: "Platform", MonitorType: "group", GroupCondition: domain.GroupConditionThreshold,
		GroupThreshold: 2, Status: domain.StatusDown, Message: "two children are down",
		TemplateTitle:  "{{ status.emoji }} {{ alert.name }} is {{ status }}",
		TemplateBody:   "{{ message }}",
		TemplateConfig: domain.DiscordTemplateConfigMap(config),
	}
	if err := (DiscordSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send structured group template: %v", err)
	}
	embed := received["embeds"].([]any)[0].(map[string]any)
	if embed["title"] != "❌ Platform is DOWN" || embed["color"] != float64(0x123456) {
		t.Fatalf("structured embed title/color = %v / %v", embed["title"], embed["color"])
	}
	if _, exists := embed["url"]; exists {
		t.Fatal("blank group title URL should be omitted")
	}
	footer := embed["footer"].(map[string]any)
	if footer["text"] != "Phoenix • group" {
		t.Fatalf("footer = %v", footer["text"])
	}
	fields := embed["fields"].([]any)
	if len(fields) != 2 {
		t.Fatalf("fields = %#v; monitor-only blank Target should be omitted", fields)
	}
	if fields[0].(map[string]any)["name"] != "Condition" || fields[1].(map[string]any)["value"] != "2" {
		t.Fatalf("unexpected group fields: %#v", fields)
	}
}

func TestDiscordSender_Send_AckURLBecomesLinkButton(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ackURL := "https://status.example.com/ack/token"
	alert := domain.AlertContext{
		MonitorName: "API", Status: domain.StatusDown,
		Message: "API is DOWN\nAcknowledge: " + ackURL,
		AckURL:  ackURL,
	}
	if err := (DiscordSender{}).Send(context.Background(), map[string]any{"webhook_url": srv.URL}, alert); err != nil {
		t.Fatalf("send: %v", err)
	}
	embed := received["embeds"].([]any)[0].(map[string]any)
	if desc, _ := embed["description"].(string); strings.Contains(desc, "Acknowledge:") {
		t.Fatalf("description still contains ack text: %q", desc)
	}
	rows := received["components"].([]any)
	buttons := rows[0].(map[string]any)["components"].([]any)
	button := buttons[0].(map[string]any)
	if button["style"] != float64(5) || button["label"] != "Acknowledge" || button["url"] != ackURL {
		t.Fatalf("ack button = %#v", button)
	}
}

func TestDiscordSender_Send_CustomAndAckButtons(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	ackURL := "https://status.example.com/ack/token"
	config := domain.DefaultDiscordTemplateConfig()
	config.Buttons = []domain.DiscordButtonTemplate{
		{LabelTemplate: "Dashboard", URLTemplate: "https://phoenix.example.com/monitors/{{ monitor.id }}"},
		{LabelTemplate: "Ack again", URLTemplate: "{{ ack_url }}"},
	}
	alert := domain.AlertContext{
		MonitorID: 42, MonitorName: "API", Status: domain.StatusDown,
		AckURL: ackURL, TemplateConfig: domain.DiscordTemplateConfigMap(config),
	}
	channelCfg := map[string]any{
		"webhook_url": srv.URL,
		"buttons": []any{
			map[string]any{"label": "Runbook", "url": "https://runbook.example.com/api"},
		},
	}
	if err := (DiscordSender{}).Send(context.Background(), channelCfg, alert); err != nil {
		t.Fatalf("send: %v", err)
	}
	rows := received["components"].([]any)
	buttons := rows[0].(map[string]any)["components"].([]any)
	if len(buttons) != 3 {
		t.Fatalf("buttons = %#v; want ack + dashboard + runbook (duplicate ack omitted)", buttons)
	}
	labels := []string{
		buttons[0].(map[string]any)["label"].(string),
		buttons[1].(map[string]any)["label"].(string),
		buttons[2].(map[string]any)["label"].(string),
	}
	if labels[0] != "Acknowledge" || labels[1] != "Dashboard" || labels[2] != "Runbook" {
		t.Fatalf("button order = %v", labels)
	}
	if buttons[1].(map[string]any)["url"] != "https://phoenix.example.com/monitors/42" {
		t.Fatalf("dashboard url = %v", buttons[1].(map[string]any)["url"])
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
