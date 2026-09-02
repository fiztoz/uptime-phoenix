import type { Handle } from "@sveltejs/kit";

const API_BASE = process.env.PHOENIX_API_URL || "http://localhost:3000";
const SITE_NAME = "Phoenix";
const SITE_DESCRIPTION =
  "Phoenix — self-hosted, K8s-native uptime monitoring and status pages.";

/** Returns a `transformPageChunk` callback that injects Open Graph / Twitter Card meta tags. */
function injectOgTags(origin: string) {
  const ogImage = `${origin}/brand/phoenix-mascot.png`;
  return ({ html }: { html: string }) =>
    html.replace(
      "<head>",
      `<head>
    <meta property="og:title" content="${SITE_NAME}" />
    <meta property="og:description" content="${SITE_DESCRIPTION}" />
    <meta property="og:image" content="${ogImage}" />
    <meta property="og:image:width" content="512" />
    <meta property="og:image:height" content="512" />
    <meta property="og:type" content="website" />
    <meta property="og:site_name" content="${SITE_NAME}" />
    <meta name="twitter:card" content="summary_large_image" />
    <meta name="twitter:title" content="${SITE_NAME}" />
    <meta name="twitter:description" content="${SITE_DESCRIPTION}" />
    <meta name="twitter:image" content="${ogImage}" />`,
    );
}

/**
 * Resolves a custom domain to a status page slug via the Phoenix API.
 * Returns the slug if found, or null if the domain is not configured.
 */
async function resolveDomainToSlug(hostname: string): Promise<string | null> {
  try {
    const res = await fetch(
      `${API_BASE}/api/status/resolve?domain=${encodeURIComponent(hostname)}`,
      {
        signal: AbortSignal.timeout(3000),
      },
    );
    if (!res.ok) return null;
    const data = await res.json();
    return data?.slug ?? null;
  } catch {
    return null;
  }
}

/**
 * Server hook for custom domain routing.
 *
 * When a request arrives at a custom domain (not localhost/internal IP),
 * this hook resolves the hostname to a status page slug via the Phoenix
 * REST API and rewrites the URL to serve the correct status page.
 */
export const handle: Handle = async ({ event, resolve }) => {
  const hostname = event.url.hostname;

  // Skip admin routes and localhost / direct IP access.
  if (
    hostname === "localhost" ||
    hostname === "127.0.0.1" ||
    hostname.startsWith("10.") ||
    hostname.startsWith("192.168.") ||
    hostname.startsWith("172.")
  ) {
    return resolve(event, {
      transformPageChunk: injectOgTags(event.url.origin),
    });
  }

  // Resolve custom domain to status page slug.
  const slug = await resolveDomainToSlug(hostname);
  if (slug) {
    // Public pages live at /status/:slug. Rewrite / → /status/:slug and
    // /history → /status/:slug/history so the custom-domain root still works.
    const prefix = `/status/${slug}`;
    if (!event.url.pathname.startsWith(prefix)) {
      const rest = event.url.pathname === "/" ? "" : event.url.pathname;
      event.url.pathname = `${prefix}${rest}`;
    }
  }

  return resolve(event, { transformPageChunk: injectOgTags(event.url.origin) });
};
