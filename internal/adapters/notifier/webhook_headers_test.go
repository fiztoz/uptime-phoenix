package notifier

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The notification form's "Extra Headers (JSON)" textarea saves headers as a
// JSON string. Send used to read only a map, so those headers (usually the
// receiver's Authorization) were silently never sent.
func TestWebhookSender_SendsHeadersSavedAsJSONString(t *testing.T) {
	for name, headers := range map[string]any{
		"json string": `{"Authorization": "Bearer from-string", "X-Team": "ops"}`,
		"object":      map[string]any{"Authorization": "Bearer from-string", "X-Team": "ops"},
	} {
		var gotAuth, gotTeam string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth, gotTeam = r.Header.Get("Authorization"), r.Header.Get("X-Team")
			w.WriteHeader(http.StatusOK)
		}))
		cfg := map[string]any{"url": srv.URL, "headers": headers}
		if err := (WebhookSender{}).Validate(cfg); err != nil {
			t.Fatalf("%s: Validate: %v", name, err)
		}
		if err := (WebhookSender{}).Send(context.Background(), cfg, domain.AlertContext{MonitorName: "m", Status: domain.StatusDown}); err != nil {
			t.Fatalf("%s: Send: %v", name, err)
		}
		srv.Close()
		if gotAuth != "Bearer from-string" || gotTeam != "ops" {
			t.Fatalf("%s: received Authorization=%q X-Team=%q; want the configured headers", name, gotAuth, gotTeam)
		}
	}
}

func TestWebhookSender_ValidateRejectsMalformedHeaders(t *testing.T) {
	for _, headers := range []any{`Authorization: Bearer x`, `{"X-Retry": 3}`, []any{"a"}} {
		if err := (WebhookSender{}).Validate(map[string]any{"url": "https://example.com/hook", "headers": headers}); err == nil {
			t.Errorf("Validate(headers=%v) = nil; want an error instead of silently dropping them", headers)
		}
	}
	if err := (WebhookSender{}).Validate(map[string]any{"url": "https://example.com/hook", "headers": "  "}); err != nil {
		t.Errorf("blank headers string: %v; want accepted as no headers", err)
	}
}
