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

// FeishuSender implements NotificationSender for Feishu/Lark webhook bot.
type FeishuSender struct{}

func init() { Register(FeishuSender{}) }

func (FeishuSender) Type() string { return "feishu" }

func (FeishuSender) Validate(config map[string]any) error {
	if v, ok := config["webhook_url"].(string); !ok || v == "" {
		return fmt.Errorf("feishu: webhook_url is required (Feishu bot webhook URL)")
	}
	return nil
}

func (FeishuSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	webhookURL, _ := config["webhook_url"].(string)

	emoji := "✅"
	color := "green"
	var headerContent string
	targetLine := ""
	if alert.MonitorTarget != "" {
		targetLine = fmt.Sprintf("**Target:** %s\n", alert.MonitorTarget)
	}
	bodyContent := fmt.Sprintf("**Monitor:** %s\n**Type:** %s\n%s**Message:** %s", alert.MonitorName, alert.MonitorType, targetLine, alert.Message)
	if isAuxiliaryAlert(alert) {
		emoji = alertEmoji(alert)
		color = "orange"
		if isCapacityCondition(alert) {
			switch alert.ConditionState {
			case domain.ConditionStateOK:
				color = "green"
			case domain.ConditionStateError:
				color = "red"
			}
		}
		headerContent = fmt.Sprintf("%s %s", emoji, alertTitle(alert))
		bodyContent = fmt.Sprintf("**Monitor:** %s\n**Type:** %s\n%s**Event:** %s\n**Message:** %s",
			alert.MonitorName, alert.MonitorType, targetLine, alert.EventKind, alertBody(alert))
	} else {
		switch alert.Status {
		case domain.StatusDown:
			emoji = "🔴"
			color = "red"
		case domain.StatusMaintenance:
			emoji = "🔧"
			color = "grey"
		case domain.StatusPending:
			emoji = "⏳"
			color = "orange"
		}
		headerContent = fmt.Sprintf("%s %s — %s", emoji, alert.MonitorName, alert.Status)
	}

	// Feishu interactive card message.
	card := map[string]any{
		"msg_type": "interactive",
		"card": map[string]any{
			"header": map[string]any{
				"title": map[string]any{
					"tag":     "plain_text",
					"content": headerContent,
				},
				"template": color,
			},
			"elements": []any{
				map[string]any{
					"tag": "div",
					"text": map[string]any{
						"tag":     "lark_md",
						"content": bodyContent,
					},
				},
			},
		},
	}
	payload, _ := json.Marshal(card)

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
		return fmt.Errorf("feishu: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("feishu: api error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
