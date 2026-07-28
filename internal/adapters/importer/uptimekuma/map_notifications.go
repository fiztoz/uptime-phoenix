package uptimekuma

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Phoenix notification providers (locked set of 11).
var phoenixProviders = map[string]struct{}{
	"telegram":   {},
	"discord":    {},
	"slack":      {},
	"smtp":       {},
	"webhook":    {},
	"teams":      {},
	"mattermost": {},
	"gotify":     {},
	"bark":       {},
	"feishu":     {},
	"line":       {},
}

// mapNotificationType maps a Kuma notification type string to a Phoenix provider.
// Returns ("", reason) when unsupported.
func mapNotificationType(kumaType string) (phoenixType string, reason string) {
	t := strings.ToLower(strings.TrimSpace(kumaType))
	switch t {
	case "telegram":
		return "telegram", ""
	case "discord":
		return "discord", ""
	case "slack":
		return "slack", ""
	case "smtp", "email", "mail":
		return "smtp", ""
	case "webhook":
		return "webhook", ""
	case "teams", "msteams", "teamswebhook", "microsoftteams":
		return "teams", ""
	case "mattermost":
		return "mattermost", ""
	case "gotify":
		return "gotify", ""
	case "bark":
		return "bark", ""
	case "feishu", "lark":
		return "feishu", ""
	case "line", "linenotify", "linemessaging", "line-messaging":
		return "line", ""
	case "":
		return "", "notification type missing"
	default:
		return "", fmt.Sprintf("unsupported notification provider %q (Phoenix supports 11 providers only)", kumaType)
	}
}

// convertNotificationConfig maps Kuma's flat camelCase config keys into
// Phoenix snake_case provider config. Secrets are preserved in the map
// (required for a restorable backup) but must never be logged.
func convertNotificationConfig(phoenixType string, raw map[string]any) map[string]any {
	out := make(map[string]any)
	get := func(keys ...string) (any, bool) {
		for _, k := range keys {
			if v, ok := raw[k]; ok && v != nil && v != "" {
				return v, true
			}
		}
		return nil, false
	}
	set := func(dst string, keys ...string) {
		if v, ok := get(keys...); ok {
			out[dst] = v
		}
	}

	switch phoenixType {
	case "telegram":
		set("bot_token", "telegramBotToken", "botToken", "bot_token")
		set("chat_id", "telegramChatID", "telegramChatId", "chatId", "chat_id")
	case "discord":
		set("webhook_url", "discordWebhookUrl", "webhookUrl", "webhook_url", "discordWebhookURL")
		set("username", "discordUsername", "username")
		set("avatar_url", "discordAvatarUrl", "avatarUrl", "avatar_url")
	case "slack":
		set("webhook_url", "slackwebhookURL", "slackWebhookURL", "webhookUrl", "webhook_url")
		set("channel", "slackchannel", "slackChannel", "channel")
	case "smtp":
		set("host", "smtpHost", "host")
		set("port", "smtpPort", "port")
		set("username", "smtpUsername", "username")
		set("password", "smtpPassword", "password")
		set("from", "smtpFrom", "from")
		set("to", "smtpTo", "to")
		if v, ok := get("smtpSecure", "smtpTLS", "use_tls", "secure"); ok {
			out["use_tls"] = v
		}
	case "webhook":
		set("url", "webhookURL", "webhookUrl", "url")
		set("method", "webhookMethod", "method")
		set("headers", "webhookHeaders", "headers")
		set("body_template", "webhookBody", "body", "body_template")
	case "teams":
		set("webhook_url", "webhookUrl", "webhookURL", "webhook_url", "teamsWebhookURL")
	case "mattermost":
		set("webhook_url", "mattermostWebhookUrl", "webhookUrl", "webhook_url")
		set("channel", "mattermostchannel", "channel")
		set("username", "mattermostusername", "username")
	case "gotify":
		set("server_url", "gotifyserverurl", "gotifyServerUrl", "serverUrl", "server_url")
		set("app_token", "gotifyapplicationToken", "gotifyApplicationToken", "applicationToken", "app_token")
	case "bark":
		set("server_url", "barkServerUrl", "serverUrl", "server_url")
		set("device_key", "barkDeviceKey", "deviceKey", "device_key")
	case "feishu":
		set("webhook_url", "feishuWebhookUrl", "webhookUrl", "webhook_url", "larkWebhookUrl")
	case "line":
		set("channel_access_token", "lineChannelAccessToken", "channelAccessToken", "channel_access_token")
		set("user_id", "lineUserID", "lineUserId", "userId", "user_id")
		set("group_id", "lineGroupID", "lineGroupId", "groupId", "group_id")
	}
	return out
}

// parseNotificationConfig unmarshals the Kuma notification.config JSON text.
// Returns the type string (from config["type"]) and the full map.
func parseNotificationConfig(configText string) (kumaType string, cfg map[string]any, err error) {
	cfg = map[string]any{}
	text := strings.TrimSpace(configText)
	if text == "" {
		return "", cfg, nil
	}
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		return "", nil, fmt.Errorf("parse notification config JSON: %w", err)
	}
	if v, ok := cfg["type"].(string); ok {
		kumaType = v
	}
	return kumaType, cfg, nil
}
