package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MattermostSender implements NotificationSender for Mattermost webhooks.
type MattermostSender struct{}

func init() { Register(MattermostSender{}) }

func (MattermostSender) Type() string { return "mattermost" }

func (MattermostSender) Validate(config map[string]any) error {
	if v, ok := config["webhook_url"].(string); !ok || v == "" {
		return fmt.Errorf("webhook_url is required")
	}
	return nil
}

func (MattermostSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	webhookURL, _ := config["webhook_url"].(string)
	channel := ""
	if c, ok := config["channel"].(string); ok && c != "" {
		channel = c
	}
	username := "Phoenix"
	if u, ok := config["username"].(string); ok && u != "" {
		username = u
	}

	color := "#808080"
	title := alertTitle(alert)
	text := alertBody(alert)
	if isCertificateExpiry(alert) {
		color = "#FFA500"
	} else {
		switch alert.Status {
		case domain.StatusUp:
			color = "#00FF00"
		case domain.StatusDown:
			color = "#FF0000"
		case domain.StatusPending:
			color = "#FFA500"
		}
		if alert.CheckOutput != "" {
			text += "\n" + alert.CheckOutput
		}
	}

	attachment := map[string]any{
		"fallback": title,
		"color":    color,
		"title":    title,
		"text":     text,
		"fields": []map[string]any{
			{"short": true, "title": "Type", "value": alert.MonitorType},
			{"short": true, "title": "Target", "value": alert.MonitorTarget},
		},
	}

	body := map[string]any{
		"username":    username,
		"text":        title,
		"attachments": []map[string]any{attachment},
	}
	if channel != "" {
		body["channel"] = channel
	}
	payload, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, webhookURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("mattermost: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("mattermost: webhook error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
