/**
 * Safe Markdown → HTML for trusted operator guides (local static files).
 * Uses marked (GFM) + DOMPurify. Never feed untrusted remote user content
 * without reviewing the sanitizer allow-list.
 */
import { marked } from "marked";
import DOMPurify from "isomorphic-dompurify";

marked.setOptions({
  gfm: true,
  breaks: false,
});

/** Tags/attrs allowed in guide previews (no scripts, no event handlers). */
const PURIFY_CONFIG = {
  USE_PROFILES: { html: true },
  ADD_ATTR: ["target", "rel", "class"],
  // Keep checkbox inputs for GFM task lists; block interactive forms/scripts.
  FORBID_TAGS: ["style", "iframe", "form", "button", "script"],
  FORBID_ATTR: ["style", "onerror", "onclick", "onload"],
};

/**
 * Parse Markdown to sanitized HTML suitable for `{@html}` in Svelte.
 */
export function renderMarkdownToSafeHtml(source: string): string {
  const md = source?.trim() ?? "";
  if (!md) return "";

  const raw = marked.parse(md, { async: false });
  const html = typeof raw === "string" ? raw : String(raw);

  // Open external links in a new tab when they look absolute.
  const withLinkSafety = html.replace(
    /<a\s+([^>]*href=["']https?:\/\/[^"']+["'][^>]*)>/gi,
    (full, attrs: string) => {
      if (/target=/i.test(attrs)) return full;
      return `<a ${attrs} target="_blank" rel="noopener noreferrer">`;
    },
  );

  // isomorphic-dompurify may return TrustedHTML in some DOM environments.
  return String(DOMPurify.sanitize(withLinkSafety, PURIFY_CONFIG));
}
