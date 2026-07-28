/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { renderMarkdownToSafeHtml } from "./markdown";

describe("renderMarkdownToSafeHtml", () => {
  test("renders headings, paragraphs, and code", () => {
    const html = renderMarkdownToSafeHtml(`# Title

A paragraph with \`inline\` code.

\`\`\`bash
echo hi
\`\`\`
`);
    expect(html).toContain("<h1");
    expect(html).toContain("Title");
    expect(html).toContain("<p>");
    expect(html).toContain("<code>");
    expect(html).toContain("<pre>");
    expect(html).toContain("echo hi");
  });

  test("renders GFM tables", () => {
    const html = renderMarkdownToSafeHtml(`| Mode | URL |
|------|-----|
| Local | unix:// |
| Remote | tcp:// |
`);
    expect(html).toContain("<table");
    expect(html).toContain("<th");
    expect(html).toContain("Remote");
  });

  test("renders lists and task items", () => {
    const html = renderMarkdownToSafeHtml(`- one
- two

- [x] done
- [ ] todo
`);
    expect(html).toContain("<ul");
    expect(html).toContain("<li");
    expect(html).toMatch(/done/);
  });

  test("strips script injection", () => {
    const html = renderMarkdownToSafeHtml(`Hello

<script>alert(1)</script>

<img src=x onerror="alert(1)">
`);
    expect(html.toLowerCase()).not.toContain("<script");
    expect(html.toLowerCase()).not.toContain("onerror");
    expect(html).toContain("Hello");
  });

  test("returns empty string for blank input", () => {
    expect(renderMarkdownToSafeHtml("")).toBe("");
    expect(renderMarkdownToSafeHtml("   ")).toBe("");
  });
});
