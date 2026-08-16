package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

	now := time.Now().UTC()
	title := alertTitle(alert)
	desc := alertBody(alert)
	custom := alert.TemplateTitle != "" || alert.TemplateBody != "" || len(alert.TemplateConfig) > 0
	if custom {
		var err error
		if alert.TemplateTitle != "" {
			title, err = domain.RenderNotificationTemplate(alert.TemplateTitle, alert, now)
			if err != nil {
				return fmt.Errorf("discord: render title: %w", err)
			}
		}
		desc, err = domain.RenderNotificationTemplate(alert.TemplateBody, alert, now)
		if err != nil {
			return fmt.Errorf("discord: render body: %w", err)
		}
	}
	if !custom && !isAuxiliaryAlert(alert) && alert.CheckOutput != "" {
		desc += "\n" + alert.CheckOutput
	}

	embedConfig, err := domain.ParseDiscordTemplateConfig(alert.TemplateConfig)
	if err != nil {
		return fmt.Errorf("discord: parse embed configuration: %w", err)
	}
	color, err := discordEmbedColor(embedConfig, alert)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	fields, fieldsLength, err := renderDiscordFields(embedConfig.Fields, alert, now)
	if err != nil {
		return fmt.Errorf("discord: %w", err)
	}
	footer, err := domain.RenderNotificationTemplate(embedConfig.FooterTemplate, alert, now)
	if err != nil {
		return fmt.Errorf("discord: render footer: %w", err)
	}
	titleURL, err := domain.RenderNotificationTemplate(embedConfig.TitleURLTemplate, alert, now)
	if err != nil {
		return fmt.Errorf("discord: render title URL: %w", err)
	}
	if titleURL != "" {
		parsed, parseErr := url.ParseRequestURI(titleURL)
		if parseErr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("discord: rendered title URL must use http or https")
		}
	}

	if utf8.RuneCountInString(title) > 256 {
		return fmt.Errorf("discord: rendered template title exceeds 256 characters")
	}
	if utf8.RuneCountInString(desc) > 4096 {
		return fmt.Errorf("discord: rendered template body exceeds 4096 characters")
	}
	if utf8.RuneCountInString(footer) > 2048 {
		return fmt.Errorf("discord: rendered template footer exceeds 2048 characters")
	}
	if utf8.RuneCountInString(titleURL) > 2048 {
		return fmt.Errorf("discord: rendered template title URL exceeds 2048 characters")
	}
	totalCharacters := utf8.RuneCountInString(title) + utf8.RuneCountInString(desc) + utf8.RuneCountInString(footer) + fieldsLength
	if totalCharacters > 6000 {
		return fmt.Errorf("discord: rendered embed exceeds 6000 total characters")
	}

	embed := map[string]any{
		"title":       title,
		"description": desc,
		"color":       color,
	}
	if len(fields) > 0 {
		embed["fields"] = fields
	}
	if titleURL != "" {
		embed["url"] = titleURL
	}
	if footer != "" {
		embed["footer"] = map[string]any{"text": footer}
	}
	if embedConfig.ShowTimestamp {
		embed["timestamp"] = now.Format(time.RFC3339)
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

func discordEmbedColor(config domain.DiscordTemplateConfig, alert domain.AlertContext) (int64, error) {
	color := config.Colors.Maintenance
	if isCertificateExpiry(alert) {
		color = config.Colors.Certificate
	} else if isCapacityCondition(alert) {
		switch alert.ConditionState {
		case domain.ConditionStateOK:
			color = config.Colors.Up
		case domain.ConditionStateError:
			color = config.Colors.Down
		default:
			color = config.Colors.Pending
		}
	} else {
		switch alert.Status {
		case domain.StatusUp:
			color = config.Colors.Up
		case domain.StatusDown:
			color = config.Colors.Down
		case domain.StatusPending:
			color = config.Colors.Pending
		}
	}
	value, err := strconv.ParseInt(strings.TrimPrefix(color, "#"), 16, 32)
	if err != nil || len(color) != 7 || !strings.HasPrefix(color, "#") {
		return 0, fmt.Errorf("embed color %q must be a six-digit hex color", color)
	}
	return value, nil
}

func renderDiscordFields(fields []domain.DiscordEmbedFieldTemplate, alert domain.AlertContext, now time.Time) ([]map[string]any, int, error) {
	if len(fields) > 25 {
		return nil, 0, fmt.Errorf("embed exceeds 25 fields")
	}
	rendered := make([]map[string]any, 0, len(fields))
	totalCharacters := 0
	for i, field := range fields {
		name, err := domain.RenderNotificationTemplate(field.NameTemplate, alert, now)
		if err != nil {
			return nil, 0, fmt.Errorf("render field %d name: %w", i+1, err)
		}
		value, err := domain.RenderNotificationTemplate(field.ValueTemplate, alert, now)
		if err != nil {
			return nil, 0, fmt.Errorf("render field %d value: %w", i+1, err)
		}
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		// Scope-specific fields deliberately disappear when their monitor.* or
		// group.* value is empty. Discord rejects empty field values.
		if name == "" || value == "" {
			continue
		}
		if utf8.RuneCountInString(name) > 256 {
			return nil, 0, fmt.Errorf("rendered field %d name exceeds 256 characters", i+1)
		}
		if utf8.RuneCountInString(value) > 1024 {
			return nil, 0, fmt.Errorf("rendered field %d value exceeds 1024 characters", i+1)
		}
		totalCharacters += utf8.RuneCountInString(name) + utf8.RuneCountInString(value)
		rendered = append(rendered, map[string]any{"name": name, "value": value, "inline": field.Inline})
	}
	return rendered, totalCharacters, nil
}
