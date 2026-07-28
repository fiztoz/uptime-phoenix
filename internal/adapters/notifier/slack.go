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

// SlackSender implements NotificationSender for Slack webhooks using Block Kit.
type SlackSender struct{}

func init() { Register(SlackSender{}) }

func (SlackSender) Type() string { return "slack" }

func (SlackSender) Validate(config map[string]any) error {
	if v, ok := config["webhook_url"].(string); !ok || v == "" {
		return fmt.Errorf("webhook_url is required")
	}
	return nil
}

func (SlackSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	webhookURL, _ := config["webhook_url"].(string)
	channel := ""
	if c, ok := config["channel"].(string); ok && c != "" {
		channel = c
	}

	var emoji, fallback, sectionText string
	if isCertificateExpiry(alert) {
		emoji = ":scroll:"
		fallback = alertTitle(alert)
		sectionText = fmt.Sprintf("*Event:* certificate_expiry\n*Message:* %s\n*Target:* %s", alertBody(alert), alert.MonitorTarget)
	} else {
		emoji = ":white_check_mark:"
		switch alert.Status {
		case domain.StatusDown:
			emoji = ":x:"
		case domain.StatusPending:
			emoji = ":warning:"
		case domain.StatusMaintenance:
			emoji = ":tools:"
		}
		fallback = fmt.Sprintf("%s %s is %s", emoji, alert.MonitorName, alert.Status)
		sectionText = fmt.Sprintf("*Status:* %s\n*Message:* %s\n*Target:* %s", alert.Status, alert.Message, alert.MonitorTarget)
	}

	blocks := []map[string]any{
		{
			"type": "header",
			"text": map[string]any{"type": "plain_text", "text": fmt.Sprintf("%s %s", emoji, alert.MonitorName)},
		},
		{
			"type": "section",
			"text": map[string]any{
				"type": "mrkdwn",
				"text": sectionText,
			},
		},
		{
			"type": "context",
			"elements": []map[string]any{
				{"type": "mrkdwn", "text": fmt.Sprintf("Monitor type: %s | %s", alert.MonitorType, time.Now().Format(time.RFC3339))},
			},
		},
	}

	body := map[string]any{
		"text":   fallback,
		"blocks": blocks,
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
		return fmt.Errorf("slack: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("slack: webhook error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
