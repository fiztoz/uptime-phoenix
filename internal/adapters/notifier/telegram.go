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

// TelegramSender implements NotificationSender for Telegram Bot API.
type TelegramSender struct{}

func init() { Register(TelegramSender{}) }

func (TelegramSender) Type() string { return "telegram" }

func (TelegramSender) Validate(config map[string]any) error {
	if v, ok := config["bot_token"].(string); !ok || v == "" {
		return fmt.Errorf("bot_token is required")
	}
	if v, ok := config["chat_id"].(string); !ok || v == "" {
		// chat_id can be numeric string or int in JSONB
		if _, ok2 := config["chat_id"].(float64); !ok2 {
			if _, ok3 := config["chat_id"].(int); !ok3 {
				return fmt.Errorf("chat_id is required")
			}
		}
	}
	return nil
}

func (TelegramSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	botToken, _ := config["bot_token"].(string)
	chatID := fmt.Sprintf("%v", config["chat_id"]) // support string or number

	var text string
	if isAuxiliaryAlert(alert) {
		text = fmt.Sprintf("%s *%s*\n%s", alertEmoji(alert), alertTitle(alert), alertBody(alert))
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
		text = fmt.Sprintf("%s *%s* is *%s*\n%s", emoji, alert.MonitorName, alert.Status, alert.Message)
		if alert.CheckOutput != "" {
			text += "\n" + alert.CheckOutput
		}
	}

	parseMode := "Markdown"
	if pm, ok := config["parse_mode"].(string); ok && pm != "" {
		parseMode = pm
	}

	body := map[string]any{
		"chat_id":    chatID,
		"text":       text,
		"parse_mode": parseMode,
	}
	payload, _ := json.Marshal(body)

	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	doReq := func(c context.Context) (*http.Response, error) {
		req, err := http.NewRequestWithContext(c, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return http.DefaultClient.Do(req)
	}

	resp, err := retryWithBackoff(ctx, doReq)
	if err != nil {
		return fmt.Errorf("telegram: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram: api error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
