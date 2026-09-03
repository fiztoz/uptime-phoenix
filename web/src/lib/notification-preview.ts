/**
 * Shared sample-data rendering for notification previews.
 * Used by the template editor and the notification detail page so both
 * surfaces show the same sample alert.
 */
import { escapeEmailHTML } from "$lib/utils/email-preview";

export type PreviewScope = "monitor" | "group";
export type PreviewStatus = "UP" | "DOWN" | "PENDING" | "MAINTENANCE";

const STATUS_EMOJI: Record<PreviewStatus, string> = {
  UP: "✅",
  DOWN: "❌",
  PENDING: "⚠️",
  MAINTENANCE: "🛠️",
};

/** Built-in Discord embed colors, matching domain.DefaultDiscordTemplateConfig. */
export const DEFAULT_DISCORD_COLORS = {
  up: "#00FF00",
  down: "#FF0000",
  pending: "#FFA500",
  maintenance: "#808080",
  certificate: "#FFA500",
} as const;

export function previewSampleValue(
  scope: PreviewScope,
  status: PreviewStatus,
  variable: string,
): unknown {
  const isGroup = scope === "group";
  const isUp = status === "UP";
  const entityName = isGroup ? "Platform Services" : "Payments API";
  const entityType = isGroup ? "group" : "http";
  const entityTarget = isGroup ? "" : "https://api.example.com/health";
  const values: Record<string, unknown> = {
    "alert.scope": scope,
    "alert.id": isGroup ? 7 : 42,
    "alert.name": entityName,
    "alert.type": entityType,
    "alert.target": entityTarget,
    "monitor.id": isGroup ? 0 : 42,
    "monitor.name": entityName,
    "monitor.type": entityType,
    "monitor.target": entityTarget,
    "monitor.description": isGroup
      ? ""
      : "Public checkout and payment authorization API",
    "monitor.owner": isGroup ? "" : "Payments on-call",
    "group.id": isGroup ? 7 : 0,
    "group.name": isGroup ? "Platform Services" : "",
    "group.description": isGroup ? "Customer-facing platform dependencies" : "",
    "group.owner": isGroup ? "Platform SRE" : "",
    "group.condition": isGroup ? "threshold" : "",
    "group.threshold": isGroup ? 2 : 0,
    "group.threshold_is_percent": false,
    "group.threshold_display": isGroup ? "2" : "",
    status,
    "status.emoji": STATUS_EMOJI[status],
    previous_status: isUp ? "DOWN" : "UP",
    message: isGroup
      ? `Group "${entityName}" is ${status}`
      : `${entityName} is ${status}`,
    check_output: isGroup
      ? isUp
        ? "All child monitors are UP"
        : "2 child monitors are DOWN"
      : isUp
        ? "200 OK • 184 ms"
        : "Request failed with status code 504",
    duration: !isGroup && isUp ? "3m12s" : "",
    started_at:
      !isGroup && (status === "DOWN" || status === "UP")
        ? "2026-08-08T02:01:00Z"
        : "",
    "started_at.unix":
      !isGroup && (status === "DOWN" || status === "UP")
        ? 1786154460
        : undefined,
    timestamp: "2026-08-08T02:04:12Z",
    "timestamp.unix": 1786154652,
    event_kind: "status_change",
    ack_url:
      !isGroup && status === "DOWN"
        ? "https://status.example.com/ack/example"
        : "",
    tags: isGroup ? {} : { team: "payments", region: "ap-southeast-1" },
    "certificate.threshold": 7,
    "certificate.days_remaining": 6,
    "certificate.issuer": "Example Trust Services",
    "certificate.not_after": "2026-08-14T00:00:00Z",
  };
  return values[variable];
}

export function renderNotificationPreview(
  source: string,
  scope: PreviewScope,
  status: PreviewStatus,
  escapeForHTML = false,
): string {
  return source.replace(
    /\{\{\s*([a-z][a-z0-9_.]*)\s*\}\}/g,
    (_match, placeholder: string) => {
      const asJSON = placeholder.startsWith("json.");
      const variable = asJSON ? placeholder.slice(5) : placeholder;
      const value = previewSampleValue(scope, status, variable);
      if (asJSON) {
        const renderedJSON = JSON.stringify(value ?? null);
        return escapeForHTML ? escapeEmailHTML(renderedJSON) : renderedJSON;
      }
      let rendered: string;
      if (value === undefined || value === null) {
        rendered = "";
      } else if (
        variable === "tags" &&
        typeof value === "object" &&
        value !== null
      ) {
        rendered = Object.entries(value as Record<string, unknown>)
          .map(([key, item]) => `${key}=${String(item)}`)
          .join(", ");
      } else {
        rendered = String(value);
      }
      return escapeForHTML ? escapeEmailHTML(rendered) : rendered;
    },
  );
}

export function safePreviewURL(value: string): string {
  try {
    const url = new URL(value);
    return url.protocol === "http:" || url.protocol === "https:"
      ? url.toString()
      : "";
  } catch {
    return "";
  }
}

export function previewEntityName(scope: PreviewScope): string {
  return scope === "group" ? "Platform Services" : "Payments API";
}

export function builtinStatusEmoji(status: PreviewStatus): string {
  return STATUS_EMOJI[status];
}
