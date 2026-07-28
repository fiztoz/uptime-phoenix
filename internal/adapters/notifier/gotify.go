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

// GotifySender implements NotificationSender for Gotify.
type GotifySender struct{}

func init() { Register(GotifySender{}) }

func (GotifySender) Type() string { return "gotify" }

func (GotifySender) Validate(config map[string]any) error {
	if v, ok := config["server_url"].(string); !ok || v == "" {
		return fmt.Errorf("server_url is required")
	}
	if v, ok := config["app_token"].(string); !ok || v == "" {
		return fmt.Errorf("app_token is required")
	}
	return nil
}

func (GotifySender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	server, _ := config["server_url"].(string)
	token, _ := config["app_token"].(string)
	priority := 5
	var title, message string
	if isCertificateExpiry(alert) {
		priority = 8
		title = alertTitleWithPrefix("Phoenix:", alert)
		message = alertBody(alert)
	} else {
		switch alert.Status {
		case domain.StatusUp:
			priority = 0
		case domain.StatusDown:
			priority = 10
		case domain.StatusPending:
			priority = 5
		case domain.StatusMaintenance:
			priority = 2
		}
		title = fmt.Sprintf("Phoenix: %s is %s", alert.MonitorName, alert.Status)
		message = alert.Message
		if alert.CheckOutput != "" {
			message += "\n" + alert.CheckOutput
		}
	}

	body := map[string]any{
		"title":    title,
		"message":  message,
		"priority": priority,
	}
	payload, _ := json.Marshal(body)

	// token in query param
	fullURL := fmt.Sprintf("%s/message?token=%s", server, url.QueryEscape(token))

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, fullURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("gotify: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gotify: api error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
