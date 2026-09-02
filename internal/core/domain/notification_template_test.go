package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestRenderNotificationTemplate_TextAndJSONVariables(t *testing.T) {
	now := time.Date(2026, time.August, 8, 4, 5, 6, 0, time.UTC)
	alert := AlertContext{
		MonitorID:      42,
		MonitorName:    `payments "primary"`,
		MonitorType:    "http",
		MonitorTarget:  "https://example.com/health",
		Status:         StatusDown,
		PreviousStatus: StatusUp,
		Message:        "connection refused",
		Tags:           map[string]string{"region": "ap-southeast-1", "team": "payments"},
	}

	rendered, err := RenderNotificationTemplate(
		`{{ monitor.name }} {{ status }} {{ tags }} {{ timestamp }} {{ json.monitor.name }}`,
		alert,
		now,
	)
	if err != nil {
		t.Fatalf("RenderNotificationTemplate: %v", err)
	}
	if !strings.Contains(rendered, `payments "primary" DOWN region=ap-southeast-1, team=payments 2026-08-08T04:05:06Z`) {
		t.Fatalf("rendered text missing alert values: %q", rendered)
	}
	if !strings.HasSuffix(rendered, `"payments \"primary\""`) {
		t.Fatalf("JSON value was not safely encoded: %q", rendered)
	}

	jsonBody, err := RenderNotificationTemplate(`{"id":{{ json.monitor.id }},"tags":{{ json.tags }}}`, alert, now)
	if err != nil {
		t.Fatalf("render JSON template: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(jsonBody), &decoded); err != nil {
		t.Fatalf("rendered JSON is invalid: %v (%s)", err, jsonBody)
	}
	if decoded["id"] != float64(42) {
		t.Fatalf("JSON monitor id = %v; want 42", decoded["id"])
	}
}

func TestValidateNotificationTemplateText_RejectsUnknownAndMalformedPlaceholders(t *testing.T) {
	for _, source := range []string{
		"{{ monitor.uptime }}",
		"{{ monitor.name }",
	} {
		if err := ValidateNotificationTemplateText(source); err == nil {
			t.Errorf("ValidateNotificationTemplateText(%q) succeeded; want error", source)
		}
	}
}

func TestNotificationTemplateProviderSupported(t *testing.T) {
	for _, provider := range []string{"discord", "smtp", "webhook", "line"} {
		if !NotificationTemplateProviderSupported(provider) {
			t.Errorf("provider %q should support templates", provider)
		}
	}
	if NotificationTemplateProviderSupported("slack") {
		t.Error("slack should not support templates in this release")
	}
}

func TestRenderNotificationTemplate_MonitorAndGroupScopes(t *testing.T) {
	now := time.Date(2026, time.August, 8, 2, 4, 12, 0, time.UTC)
	groupAlert := AlertContext{
		AlertScope:     AlertScopeGroup,
		MonitorName:    "Platform Services", // legacy group-template compatibility
		MonitorType:    "group",
		GroupID:        7,
		GroupName:      "Platform Services",
		GroupCondition: GroupConditionThreshold,
		GroupThreshold: 2,
		Status:         StatusDown,
		StartedAt:      time.Date(2026, time.August, 8, 2, 1, 0, 0, time.UTC),
	}
	rendered, err := RenderNotificationTemplate(
		`{{ alert.scope }}|{{ alert.id }}|{{ alert.name }}|{{ group.condition }}|{{ group.threshold_display }}|{{ status.emoji }}|{{ started_at.unix }}|{{ timestamp.unix }}`,
		groupAlert,
		now,
	)
	if err != nil {
		t.Fatalf("render group template: %v", err)
	}
	if rendered != "group|7|Platform Services|threshold|2|❌|1786154460|1786154652" {
		t.Fatalf("rendered group values = %q", rendered)
	}
}

func TestRenderNotificationTemplate_UnknownLifecycleValuesStayEmpty(t *testing.T) {
	rendered, err := RenderNotificationTemplate(
		`{{ duration }}|{{ started_at }}|{{ started_at.unix }}|{{ ack_url }}|{{ tags }}|{{ json.started_at.unix }}`,
		AlertContext{},
		time.Unix(0, 0).UTC(),
	)
	if err != nil {
		t.Fatalf("RenderNotificationTemplate: %v", err)
	}
	if rendered != "|||||null" {
		t.Fatalf("unknown lifecycle values = %q; want empty fields", rendered)
	}
}

func TestRenderNotificationTemplate_CapacityConditionVariables(t *testing.T) {
	used, limit, percent, threshold := 84.0, 100.0, 84.0, 80.0
	observedAt := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	alert := AlertContext{
		EventKind:     AlertEventCapacityCondition,
		ConditionKind: MonitorConditionStorage, ConditionState: ConditionStateWarning,
		ConditionPreviousState: ConditionStateOK, ConditionUsed: &used, ConditionLimit: &limit,
		ConditionPercent: &percent, ConditionThreshold: &threshold, ConditionUnit: "bytes",
		ConditionResource: "Database size", ConditionScope: "database", ConditionSource: "fixed query",
		ConditionObservedAt: &observedAt,
	}
	rendered, err := RenderNotificationTemplate(
		`{{ status.emoji }}|{{ condition.kind }}|{{ condition.state }}|{{ condition.previous_state }}|{{ condition.used }}|{{ condition.limit }}|{{ condition.percent }}|{{ condition.threshold }}|{{ condition.unit }}|{{ condition.resource }}|{{ condition.scope }}|{{ condition.source }}|{{ condition.observed_at }}|{{ json.condition.percent }}`,
		alert,
		time.Unix(0, 0).UTC(),
	)
	if err != nil {
		t.Fatalf("render capacity template: %v", err)
	}
	want := "⚠️|storage|warning|ok|84|100|84|80|bytes|Database size|database|fixed query|2031-02-03T04:05:06Z|84"
	if rendered != want {
		t.Fatalf("rendered capacity values=%q want=%q", rendered, want)
	}
}

func TestDiscordTemplateConfig_RoundTripAndDefaults(t *testing.T) {
	defaults, err := ParseDiscordTemplateConfig(nil)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if !defaults.ShowTimestamp || defaults.Colors.Down != "#FF0000" || len(defaults.Fields) == 0 {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	want := defaults
	want.TitleURLTemplate = "https://example.com/{{ monitor.id }}"
	want.FooterTemplate = "Phoenix • {{ alert.scope }}"
	want.ShowTimestamp = false
	want.Fields = []DiscordEmbedFieldTemplate{}
	want.Buttons = []DiscordButtonTemplate{
		{LabelTemplate: "Acknowledge", URLTemplate: "{{ ack_url }}"},
	}
	got, err := ParseDiscordTemplateConfig(DiscordTemplateConfigMap(want))
	if err != nil {
		t.Fatalf("round-trip config: %v", err)
	}
	if got.TitleURLTemplate != want.TitleURLTemplate || got.FooterTemplate != want.FooterTemplate || got.ShowTimestamp || len(got.Fields) != 0 {
		t.Fatalf("round-trip config = %+v; want %+v", got, want)
	}
	if len(got.Buttons) != 1 || got.Buttons[0].URLTemplate != "{{ ack_url }}" {
		t.Fatalf("round-trip buttons = %+v", got.Buttons)
	}

	if _, err := ParseDiscordTemplateConfig(map[string]any{"fields": "invalid"}); err == nil {
		t.Fatal("malformed fields should fail")
	}
}

func TestSMTPTemplateConfig_RoundTripAndDefaults(t *testing.T) {
	defaults, err := ParseSMTPTemplateConfig(nil)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if defaults.Format != SMTPTemplateFormatPlain || defaults.HTMLBodyTemplate != "" {
		t.Fatalf("unexpected defaults: %+v", defaults)
	}

	want := SMTPTemplateConfig{
		Format:           SMTPTemplateFormatHTML,
		HTMLBodyTemplate: "<strong>{{ alert.name }}</strong>",
	}
	got, err := ParseSMTPTemplateConfig(SMTPTemplateConfigMap(want))
	if err != nil {
		t.Fatalf("round-trip config: %v", err)
	}
	if got != want {
		t.Fatalf("round-trip config = %+v; want %+v", got, want)
	}

	for _, values := range []map[string]any{
		{"format": "markdown"},
		{"format": true},
		{"html_body_template": false},
	} {
		if _, err := ParseSMTPTemplateConfig(values); err == nil {
			t.Errorf("ParseSMTPTemplateConfig(%#v) succeeded; want error", values)
		}
	}
}

func TestRenderNotificationHTMLTemplate_ContextEscapesAlertValues(t *testing.T) {
	alert := AlertContext{
		MonitorName:   `<script>alert("name")</script>`,
		MonitorTarget: "https://example.test/health?a=1&b=2",
		Message:       `<img src=x onerror="alert(1)">`,
	}
	rendered, err := RenderNotificationHTMLTemplate(
		`<h1>{{ monitor.name }}</h1><p data-message="{{ message }}">{{ message }}</p><a href="{{ monitor.target }}">Open</a>`,
		alert,
		time.Unix(0, 0).UTC(),
	)
	if err != nil {
		t.Fatalf("RenderNotificationHTMLTemplate: %v", err)
	}
	if strings.Contains(rendered, "<script>") || strings.Contains(rendered, "<img") {
		t.Fatalf("rendered HTML contains unescaped alert markup: %s", rendered)
	}
	for _, want := range []string{
		`&lt;script&gt;alert(&#34;name&#34;)&lt;/script&gt;`,
		`&lt;img src=x onerror=&#34;alert(1)&#34;&gt;`,
		`https://example.test/health?a=1&amp;b=2`,
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered HTML missing %q: %s", want, rendered)
		}
	}
}

func TestRenderNotificationHTMLTemplate_RejectsUnsafeURLAndMalformedPlaceholder(t *testing.T) {
	alert := AlertContext{MonitorTarget: "javascript:alert(document.cookie)"}
	if _, err := RenderNotificationHTMLTemplate(
		`<a href="{{ monitor.target }}">Open</a>`, alert, time.Unix(0, 0).UTC(),
	); err == nil || !strings.Contains(err.Error(), "unsafe value") {
		t.Fatalf("unsafe URL error = %v; want unsafe value rejection", err)
	}
	if _, err := RenderNotificationHTMLTemplate(
		`<p>{{ monitor.name }</p>`, alert, time.Unix(0, 0).UTC(),
	); err == nil {
		t.Fatal("malformed HTML placeholder should fail")
	}
}
