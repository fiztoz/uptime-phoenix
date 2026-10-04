package notifier

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestRegionalNotificationAttribution(t *testing.T) {
	alert := domain.AlertContext{MonitorName: "Checkout", MonitorType: "http", Status: domain.StatusDown, Message: "timeout", ProbeID: "remote", ProbeName: "Singapore", ProbeLocation: "Asia", DeliveryScope: domain.IncidentScopeRegional, SourceAlertID: "source-incident"}
	for _, name := range []string{"telegram", "discord", "slack", "webhook", "teams", "mattermost", "gotify", "bark", "feishu", "line"} {
		t.Run(name, func(t *testing.T) {
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				requests <- string(body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"ok":true,"code":200}`))
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			installLineTransport(t, &lineTestTransport{target: target})
			sender, ok := Get(name)
			if !ok {
				t.Fatal(name)
			}
			config := map[string]any{"webhook_url": server.URL, "url": server.URL, "server_url": server.URL, "device_key": "test", "app_token": "test", "bot_token": "test", "chat_id": "test", "channel_access_token": "test", "user_id": "test"}
			if err := sender.Send(t.Context(), config, alert); err != nil {
				t.Fatal(err)
			}
			body := <-requests
			for _, want := range []string{"Singapore", "Asia", "timeout"} {
				if !strings.Contains(body, want) {
					t.Fatalf("missing %s: %s", want, body)
				}
			}
			if name == "webhook" && !strings.Contains(body, `"source_alert_id":"source-incident"`) {
				t.Fatal("missing source identity", body)
			}
		})
	}
	server := newFakeSMTPServer(t)
	host, port := server.hostPort(t)
	if err := (SMTPSender{}).Send(t.Context(), map[string]any{"host": host, "port": port, "from": "phoenix@example.test", "to": "sink@example.test", "tls": false}, alert); err != nil {
		t.Fatal(err)
	}
	messages := server.received()
	if len(messages) != 1 {
		t.Fatal("SMTP message missing")
	}
	body := decodeQPBody(t, messages[0].Data)
	if !strings.Contains(body, "Singapore") || !strings.Contains(body, "Asia") {
		t.Fatal("SMTP region missing", body)
	}
}
