/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  previewSampleValue,
  renderNotificationPreview,
  safePreviewURL,
} from "./notification-preview";

describe("notification preview renderer", () => {
  test("fills monitor DOWN sample values", () => {
    expect(previewSampleValue("monitor", "DOWN", "alert.name")).toBe(
      "Payments API",
    );
    expect(previewSampleValue("monitor", "DOWN", "status")).toBe("DOWN");
    expect(previewSampleValue("monitor", "DOWN", "ack_url")).toContain("/ack/");
    expect(previewSampleValue("monitor", "UP", "ack_url")).toBe("");
  });

  test("renders template placeholders and JSON values", () => {
    const rendered = renderNotificationPreview(
      "{{ alert.name }} is {{ status }} {{ json.event_kind }}",
      "monitor",
      "DOWN",
    );
    expect(rendered).toBe('Payments API is DOWN "status_change"');
  });

  test("escapes values for HTML email insertion", () => {
    const rendered = renderNotificationPreview(
      "<p>{{ json.message }}</p>",
      "monitor",
      "DOWN",
      true,
    );
    expect(rendered).toContain("&quot;");
    expect(rendered).not.toContain('{"');
  });

  test("rejects non-http preview URLs", () => {
    expect(safePreviewURL("https://api.example.com/health")).toBe(
      "https://api.example.com/health",
    );
    expect(safePreviewURL("javascript:alert(1)")).toBe("");
  });
});
