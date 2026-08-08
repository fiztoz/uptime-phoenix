/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  buildEmailPreviewDocument,
  escapeEmailHTML,
  resolveEmailPreviewSubject,
  sanitizeEmailPreviewHTML,
} from "./email-preview";

describe("email preview safety", () => {
  test("escapes rendered alert values before inserting them into HTML", () => {
    expect(escapeEmailHTML(`<img src=x onerror="alert(1)">&'`)).toBe(
      "&lt;img src=x onerror=&quot;alert(1)&quot;&gt;&amp;&#39;",
    );
  });

  test("uses the SMTP fallback when an authored subject renders empty", () => {
    expect(resolveEmailPreviewSubject("   ", "Platform Services", "DOWN")).toBe(
      "Phoenix Alert: Platform Services is DOWN",
    );
    expect(
      resolveEmailPreviewSubject("Custom subject", "Payments API", "UP"),
    ).toBe("Custom subject");
  });

  test("keeps email tables and inline styles", () => {
    const html = sanitizeEmailPreviewHTML(
      '<table role="presentation" style="width:100%"><tr><td>Alert</td></tr></table>',
    );
    expect(html).toContain("<table");
    expect(html).toContain('style="width:100%"');
    expect(html).toContain("Alert");
  });

  test("removes active and form content", () => {
    const html = sanitizeEmailPreviewHTML(
      '<script>alert(1)</script><form><input value="secret"></form><iframe src="https://example.com"></iframe><p onclick="alert(2)">Safe</p>',
    );
    expect(html.toLowerCase()).not.toContain("script");
    expect(html.toLowerCase()).not.toContain("form");
    expect(html.toLowerCase()).not.toContain("input");
    expect(html.toLowerCase()).not.toContain("iframe");
    expect(html.toLowerCase()).not.toContain("onclick");
    expect(html).toContain("Safe");
  });

  test("blocks remote resources and makes links inert in the preview document", () => {
    const document = buildEmailPreviewDocument(
      '<img src="https://tracker.example/pixel.gif"><a href="https://example.com">Details</a>',
    );
    expect(document).toContain("img-src data: cid:");
    expect(document).toContain("connect-src 'none'");
    expect(document).toContain("pointer-events: none !important");
    expect(document).not.toContain('href="https://example.com"');
    expect(document).toContain("Details");
  });
});
