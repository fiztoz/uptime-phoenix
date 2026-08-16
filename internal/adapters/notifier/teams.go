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

// TeamsSender implements NotificationSender for Microsoft Teams webhooks (MessageCard).
type TeamsSender struct{}

func init() { Register(TeamsSender{}) }

func (TeamsSender) Type() string { return "teams" }

func (TeamsSender) Validate(config map[string]any) error {
	if v, ok := config["webhook_url"].(string); !ok || v == "" {
		return fmt.Errorf("webhook_url is required")
	}
	return nil
}

func (TeamsSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	webhookURL, _ := config["webhook_url"].(string)
	themeColor := "808080"
	var title, text string
	var facts []map[string]any
	if isAuxiliaryAlert(alert) {
		themeColor = "FFA500"
		if isCapacityCondition(alert) {
			switch alert.ConditionState {
			case domain.ConditionStateOK:
				themeColor = "00FF00"
			case domain.ConditionStateError:
				themeColor = "FF0000"
			}
		}
		title = alertTitleWithPrefix("Phoenix Alert:", alert)
		text = fmt.Sprintf("%s\nTarget: %s (%s)", alertBody(alert), alert.MonitorTarget, alert.MonitorType)
		facts = []map[string]any{
			{"name": "Event", "value": alert.EventKind},
			{"name": "Monitor", "value": alert.MonitorName},
			{"name": "Time", "value": time.Now().Format(time.RFC3339)},
		}
		if isCertificateExpiry(alert) {
			facts = append(facts, map[string]any{"name": "Days remaining", "value": fmt.Sprintf("%d", alert.CertDaysRemaining)})
		} else {
			facts = append(facts,
				map[string]any{"name": "Condition", "value": alert.ConditionKind},
				map[string]any{"name": "State", "value": alert.ConditionState},
			)
		}
	} else {
		switch alert.Status {
		case domain.StatusUp:
			themeColor = "00FF00"
		case domain.StatusDown:
			themeColor = "FF0000"
		case domain.StatusPending:
			themeColor = "FFA500"
		case domain.StatusMaintenance:
			themeColor = "808080"
		}
		title = fmt.Sprintf("Phoenix Alert: %s is %s", alert.MonitorName, alert.Status)
		text = fmt.Sprintf("%s\nTarget: %s (%s)", alert.Message, alert.MonitorTarget, alert.MonitorType)
		if alert.CheckOutput != "" {
			text += "\n" + alert.CheckOutput
		}
		facts = []map[string]any{
			{"name": "Status", "value": alert.Status.String()},
			{"name": "Monitor", "value": alert.MonitorName},
			{"name": "Time", "value": time.Now().Format(time.RFC3339)},
		}
	}

	sections := []map[string]any{
		{
			"activityTitle": title,
			"facts":         facts,
		},
	}

	body := map[string]any{
		"@type":      "MessageCard",
		"@context":   "http://schema.org/extensions",
		"themeColor": themeColor,
		"summary":    title,
		"title":      title,
		"text":       text,
		"sections":   sections,
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
		return fmt.Errorf("teams: sending message: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("teams: webhook error status %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
