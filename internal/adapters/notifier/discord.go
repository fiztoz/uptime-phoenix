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

// DiscordSender implements NotificationSender for Discord webhooks.
type DiscordSender struct{}

func init() { Register(DiscordSender{}) }

func (DiscordSender) Type() string { return "discord" }

func (DiscordSender) Validate(config map[string]any) error {
	if v, ok := config["webhook_url"].(string); !ok || v == "" {
		return fmt.Errorf("webhook_url is required")
	}
	return nil
}

func (DiscordSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	webhookURL, _ := config["webhook_url"].(string)
	username := "Phoenix"
	if u, ok := config["username"].(string); ok && u != "" {
		username = u
	}
	avatarURL, _ := config["avatar_url"].(string)

	color := 0x808080 // maintenance gray
	title := alertTitle(alert)
	desc := alertBody(alert)
	if isCertificateExpiry(alert) {
		color = 0xFFA500 // amber for certificate warnings
	} else {
		switch alert.Status {
		case domain.StatusUp:
			color = 0x00FF00
		case domain.StatusDown:
			color = 0xFF0000
		case domain.StatusPending:
			color = 0xFFA500
		}
		if alert.CheckOutput != "" {
			desc += "\n" + alert.CheckOutput
		}
	}

	embed := map[string]any{
		"title":       title,
		"description": desc,
		"color":       color,
		"fields": []map[string]any{
			{"name": "Monitor", "value": alert.MonitorName, "inline": true},
			{"name": "Type", "value": alert.MonitorType, "inline": true},
			{"name": "Target", "value": alert.MonitorTarget, "inline": true},
		},
		"timestamp": time.Now().Format(time.RFC3339),
	}

	body := map[string]any{
		"username": username,
		"embeds":   []map[string]any{embed},
	}
	if avatarURL != "" {
		body["avatar_url"] = avatarURL
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
		return fmt.Errorf("discord: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("discord: webhook error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
