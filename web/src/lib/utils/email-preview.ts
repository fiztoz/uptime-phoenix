import DOMPurify from "isomorphic-dompurify";

const EMAIL_PURIFY_CONFIG = {
  USE_PROFILES: { html: true },
  FORBID_TAGS: [
    "script",
    "iframe",
    "object",
    "embed",
    "form",
    "input",
    "button",
    "textarea",
    "select",
    "option",
    "video",
    "audio",
    "source",
    "track",
    "svg",
    "math",
    "base",
    "link",
    "meta",
  ],
  // Strip link destinations entirely: CSS pointer-events alone does not stop
  // keyboard activation from navigating the sandboxed preview frame.
  FORBID_ATTR: ["href", "srcset", "formaction", "ping"],
};

/** Escape a rendered template value before inserting it into authored email HTML. */
export function escapeEmailHTML(value: string): string {
  return value
    .replaceAll("&", "&amp;")
    .replaceAll("<", "&lt;")
    .replaceAll(">", "&gt;")
    .replaceAll('"', "&quot;")
    .replaceAll("'", "&#39;");
}

/** Match the SMTP sender's default when an authored subject renders empty. */
export function resolveEmailPreviewSubject(
  renderedSubject: string,
  alertName: string,
  status: string,
): string {
  if (renderedSubject.trim()) return renderedSubject;
  return `Phoenix Alert: ${alertName} is ${status}`;
}

/**
 * Sanitize operator-authored email markup for the browser preview. Delivery
 * validation remains a backend concern; this function protects the Phoenix UI.
 */
export function sanitizeEmailPreviewHTML(source: string): string {
  return String(DOMPurify.sanitize(source, EMAIL_PURIFY_CONFIG));
}

/** Build an isolated email document with networking and interaction disabled. */
export function buildEmailPreviewDocument(source: string): string {
  const sanitized = sanitizeEmailPreviewHTML(source);
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="Content-Security-Policy" content="default-src 'none'; base-uri 'none'; form-action 'none'; frame-src 'none'; connect-src 'none'; font-src data:; img-src data: cid:; style-src 'unsafe-inline'">
  <style>
    html { color-scheme: light; background: #f3f4f6; }
    body { margin: 0; min-width: 0; background: #f3f4f6; color: #1f2937; font-family: Arial, Helvetica, sans-serif; overflow-wrap: anywhere; }
    img { max-width: 100%; height: auto; }
    table { max-width: 100%; }
    a { pointer-events: none !important; cursor: default !important; }
  </style>
</head>
<body>${sanitized}</body>
</html>`;
}
