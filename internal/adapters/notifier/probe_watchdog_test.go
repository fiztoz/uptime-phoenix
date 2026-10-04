package notifier

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func probeProviderAlert(status domain.Status) domain.AlertContext {
	return domain.AlertContext{AlertScope: domain.AlertScopeProbe, DeliveryScope: domain.IncidentScopeProbeConnection, EventKind: domain.DeliveryEventProbeConnection, ProbeID: "22222222-2222-4222-8222-222222222222", ProbeName: "Bangkok edge", ProbeLocation: "Thailand", SourceAlertID: "33333333-3333-4333-8333-333333333333", Status: status, Message: "Application health changed"}
}

func TestProbeWatchdogHTTPProviderRequests(t *testing.T) {
	for _, name := range []string{"telegram", "discord", "slack", "webhook", "teams", "mattermost", "gotify", "bark", "feishu", "line"} {
		t.Run(name, func(t *testing.T) {
			requests := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				requests <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"code":200}`))
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			// All HTTP traffic, including fixed Telegram/LINE URLs, is redirected
			// to this local server. No real recipient is contacted.
			installLineTransport(t, &lineTestTransport{target: target})
			sender, ok := Get(name)
			if !ok {
				t.Fatal("missing existing sender", name)
			}
			config := map[string]any{"webhook_url": server.URL, "url": server.URL, "server_url": server.URL, "device_key": "test", "app_token": "test", "bot_token": "test", "chat_id": "test", "channel_access_token": "test", "user_id": "test"}
			if err := sender.Validate(config); err != nil {
				t.Fatal(err)
			}
			for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
				alert := probeProviderAlert(status)
				if name == "slack" {
					alert.ProbeName = strings.Repeat("e", 200)
				}
				if err := sender.Send(t.Context(), config, alert); err != nil {
					t.Fatal(err)
				}
				body := <-requests
				var payload any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal("provider did not receive JSON", err)
				}
				text := string(body)
				want := "connection lost"
				if status == domain.StatusUp {
					want = "connection restored"
				}
				for _, value := range []string{want, alert.ProbeName, alert.ProbeLocation} {
					if !strings.Contains(text, value) {
						t.Fatalf("%s missing %q: %s", name, value, text)
					}
				}
				if strings.Contains(strings.ToLower(text), "monitor") || strings.Contains(text, `"Condition"`) {
					t.Fatalf("probe alert fabricated monitor/condition: %s", text)
				}
				if name == "gotify" && status == domain.StatusUp && payload.(map[string]any)["priority"] != float64(0) {
					t.Fatal("connection recovery remained urgent", payload)
				}
				if name == "slack" {
					header := payload.(map[string]any)["blocks"].([]any)[0].(map[string]any)["text"].(map[string]any)["text"].(string)
					if len([]rune(header)) > 150 {
						t.Fatal("probe name exceeded Slack header limit", len([]rune(header)))
					}
				}
				if name == "webhook" {
					value := payload.(map[string]any)
					probeValue, ok := value["probe"].(map[string]any)
					if !ok || probeValue["id"] != alert.ProbeID || value["source_alert_id"] != alert.SourceAlertID || value["delivery_scope"] != string(domain.IncidentScopeProbeConnection) {
						t.Fatal("webhook lost explicit source/probe identity", value)
					}
				}
			}
		})
	}
}

func TestProbeWatchdogSMTPMessage(t *testing.T) {
	server := newFakeSMTPServer(t)
	host, port := server.hostPort(t)
	config := map[string]any{"host": host, "port": port, "from": "phoenix@example.test", "to": "sink@example.test", "tls": false}
	for _, status := range []domain.Status{domain.StatusDown, domain.StatusUp} {
		if err := (SMTPSender{}).Send(t.Context(), config, probeProviderAlert(status)); err != nil {
			t.Fatal(err)
		}
	}
	messages := server.received()
	if len(messages) != 2 {
		t.Fatal("missing SMTP transactions")
	}
	for i, message := range messages {
		body := decodeQPBody(t, message.Data)
		if !strings.Contains(body, "Bangkok edge") || !strings.Contains(body, "Thailand") || strings.Contains(body, "Monitor:") {
			t.Fatal("wrong SMTP entity", body)
		}
		want := "connection lost"
		if i == 1 {
			want = "connection restored"
		}
		if !strings.Contains(message.Data, want) {
			t.Fatal("wrong SMTP subject", message.Data)
		}
	}
}
