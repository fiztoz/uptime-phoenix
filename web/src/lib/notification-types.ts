/**
 * Per-notification-type config field definitions for dynamic forms.
 * Used by NotificationForm to render conditional inputs per provider type.
 */
export type FieldType = "text" | "number" | "select" | "textarea" | "password";

export interface ConfigField {
  key: string;
  label: string;
  type: FieldType;
  required?: boolean;
  placeholder?: string;
  default?: string | number;
  options?: { value: string; label: string }[];
  help?: string;
}

export const notificationTypeConfig: Record<
  string,
  { label: string; fields: ConfigField[] }
> = {
  telegram: {
    label: "Telegram",
    fields: [
      {
        key: "bot_token",
        label: "Bot Token",
        type: "password",
        required: true,
        placeholder: "123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11",
      },
      {
        key: "chat_id",
        label: "Chat ID",
        type: "text",
        required: true,
        placeholder: "-1001234567890",
        help: "Use @userinfobot or negative for groups",
      },
    ],
  },
  discord: {
    label: "Discord",
    fields: [
      {
        key: "webhook_url",
        label: "Webhook URL",
        type: "password",
        required: true,
        placeholder: "https://discord.com/api/webhooks/...",
      },
      {
        key: "username",
        label: "Bot Username",
        type: "text",
        default: "Phoenix Monitor",
      },
      {
        key: "avatar_url",
        label: "Avatar URL",
        type: "text",
        placeholder: "https://example.com/avatar.png",
        help: "URL to a PNG/JPG for the bot avatar",
      },
    ],
  },
  slack: {
    label: "Slack",
    fields: [
      {
        key: "webhook_url",
        label: "Webhook URL",
        type: "password",
        required: true,
        placeholder: "https://hooks.slack.com/services/...",
      },
      {
        key: "channel",
        label: "Channel",
        type: "text",
        placeholder: "#alerts",
      },
    ],
  },
  smtp: {
    label: "SMTP Email",
    fields: [
      {
        key: "host",
        label: "SMTP Host",
        type: "text",
        required: true,
        placeholder: "smtp.gmail.com",
      },
      { key: "port", label: "Port", type: "number", default: 587 },
      { key: "username", label: "Username", type: "text", required: true },
      { key: "password", label: "Password", type: "password", required: true },
      {
        key: "from",
        label: "From Address",
        type: "text",
        required: true,
        placeholder: "alerts@example.com",
      },
      {
        key: "to",
        label: "To Address(es)",
        type: "text",
        required: true,
        placeholder: "ops@example.com,admin@example.com",
        help: "Comma separated",
      },
      {
        key: "use_tls",
        label: "Use TLS",
        type: "select",
        default: "true",
        options: [
          { value: "true", label: "Yes" },
          { value: "false", label: "No" },
        ],
      },
    ],
  },
  webhook: {
    label: "Webhook",
    fields: [
      {
        key: "url",
        label: "URL",
        type: "text",
        required: true,
        placeholder: "https://example.com/webhook",
      },
      {
        key: "method",
        label: "Method",
        type: "select",
        default: "POST",
        options: [
          { value: "POST", label: "POST" },
          { value: "PUT", label: "PUT" },
          { value: "PATCH", label: "PATCH" },
        ],
      },
      {
        key: "headers",
        label: "Extra Headers (JSON)",
        type: "textarea",
        placeholder: '{"Authorization": "Bearer xxx"}',
      },
      {
        key: "body_template",
        label: "Body Template",
        type: "textarea",
        help: "Use {{monitor.name}}, {{status}} etc.",
      },
    ],
  },
  teams: {
    label: "Microsoft Teams",
    fields: [
      {
        key: "webhook_url",
        label: "Webhook URL",
        type: "password",
        required: true,
        placeholder: "https://outlook.office.com/webhook/...",
      },
    ],
  },
  mattermost: {
    label: "Mattermost",
    fields: [
      {
        key: "webhook_url",
        label: "Webhook URL",
        type: "password",
        required: true,
      },
      { key: "channel", label: "Channel", type: "text", placeholder: "alerts" },
      { key: "username", label: "Username", type: "text", default: "Phoenix" },
    ],
  },
  gotify: {
    label: "Gotify",
    fields: [
      {
        key: "server_url",
        label: "Server URL",
        type: "text",
        required: true,
        placeholder: "https://gotify.example.com",
      },
      {
        key: "app_token",
        label: "App Token",
        type: "password",
        required: true,
      },
      { key: "priority", label: "Priority", type: "number", default: 5 },
    ],
  },
  bark: {
    label: "Bark (iOS)",
    fields: [
      {
        key: "server_url",
        label: "Server URL",
        type: "text",
        required: true,
        default: "https://api.day.app",
      },
      {
        key: "device_key",
        label: "Device Key",
        type: "password",
        required: true,
      },
    ],
  },
  feishu: {
    label: "Feishu / Lark",
    fields: [
      {
        key: "webhook_url",
        label: "Webhook URL",
        type: "password",
        required: true,
        placeholder: "https://open.feishu.cn/open-apis/bot/v2/hook/...",
      },
    ],
  },
  line: {
    label: "LINE",
    fields: [
      {
        key: "channel_access_token",
        label: "Channel Access Token",
        type: "password",
        required: true,
      },
      {
        key: "user_id",
        label: "User ID",
        type: "text",
        placeholder: "U1234567890abcdef",
      },
      {
        key: "group_id",
        label: "Group ID (alt)",
        type: "text",
        placeholder: "C1234567890abcdef",
      },
      {
        key: "room_id",
        label: "Room ID (alt)",
        type: "text",
        placeholder: "R1234567890abcdef",
      },
    ],
  },
};

export const notificationTypes = Object.keys(notificationTypeConfig);
