package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// BarkSender implements NotificationSender for Bark (iOS push notification app).
type BarkSender struct{}

func init() { Register(BarkSender{}) }

func (BarkSender) Type() string { return "bark" }

func (BarkSender) Validate(config map[string]any) error {
	if v, ok := config["server_url"].(string); !ok || v == "" {
		return fmt.Errorf("bark: server_url is required (e.g. https://api.day.app)")
	}
	if v, ok := config["device_key"].(string); !ok || v == "" {
		return fmt.Errorf("bark: device_key is required")
	}
	return nil
}

func (BarkSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	serverURL, _ := config["server_url"].(string)
	deviceKey, _ := config["device_key"].(string)

	var title, body string
	if isAuxiliaryAlert(alert) {
		title = fmt.Sprintf("%s %s", alertEmoji(alert), alertTitle(alert))
		body = alertBody(alert)
	} else {
		emoji := "✅"
		switch alert.Status {
		case domain.StatusDown:
			emoji = "🔴"
		case domain.StatusMaintenance:
			emoji = "🔧"
		case domain.StatusPending:
			emoji = "⏳"
		}
		title = fmt.Sprintf("%s %s", emoji, alert.MonitorName)
		body = fmt.Sprintf("Status: %s\n%s", alert.Status, alert.Message)
	}

	// Bark supports both GET and POST. Use POST for reliability.
	payload, _ := json.Marshal(map[string]any{
		"title": title,
		"body":  body,
		"group": "Phoenix",
		"icon":  "",
		"sound": "",
		"url":   "",
	})

	apiURL := fmt.Sprintf("%s/%s", serverURL, url.PathEscape(deviceKey))

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, apiURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("bark: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bark: api error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
