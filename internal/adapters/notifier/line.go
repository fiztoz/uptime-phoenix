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

// LineSender implements NotificationSender for LINE Messaging API (push message).
type LineSender struct{}

func init() { Register(LineSender{}) }

func (LineSender) Type() string { return "line" }

func (LineSender) Validate(config map[string]any) error {
	if v, ok := config["channel_access_token"].(string); !ok || v == "" {
		return fmt.Errorf("line: channel_access_token is required")
	}
	if v, ok := config["user_id"].(string); !ok || v == "" {
		// Also accept group_id or room_id
		if _, ok2 := config["group_id"].(string); !ok2 {
			if _, ok3 := config["room_id"].(string); !ok3 {
				return fmt.Errorf("line: user_id, group_id, or room_id is required")
			}
		}
	}
	return nil
}

func (LineSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	channelToken, _ := config["channel_access_token"].(string)

	var text string
	if isCertificateExpiry(alert) {
		text = fmt.Sprintf("📜 %s\n%s", alertTitle(alert), alertBody(alert))
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
		text = fmt.Sprintf("%s %s is %s\n%s", emoji, alert.MonitorName, alert.Status, alert.Message)
	}
	_, customBody, custom, err := renderCustomLayout(alert)
	if err != nil {
		return fmt.Errorf("line: %w", err)
	}
	if custom {
		text = customBody
	}
	if len([]rune(text)) > 5000 {
		return fmt.Errorf("line: rendered template body exceeds 5000 characters")
	}

	// Build the push message target.
	to := map[string]any{}
	if uid, ok := config["user_id"].(string); ok && uid != "" {
		to["to"] = uid
	} else if gid, ok := config["group_id"].(string); ok && gid != "" {
		to["to"] = gid
	} else if rid, ok := config["room_id"].(string); ok && rid != "" {
		to["to"] = rid
	}

	body := map[string]any{
		"to": to["to"],
		"messages": []any{
			map[string]any{
				"type": "text",
				"text": text,
			},
		},
	}
	payload, _ := json.Marshal(body)

	const lineAPIURL = "https://api.line.me/v2/bot/message/push"

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, lineAPIURL, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+channelToken)
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("line: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("line: api error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
