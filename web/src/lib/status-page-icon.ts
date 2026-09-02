/** Kuma ships public/icon.svg as the default status-page logo. Phoenix does not. */
const KUMA_DEFAULT_ICON_PATHS = new Set([
  "/icon.svg",
  "/icon.png",
  "/icon.ico",
]);

function pathOf(icon: string): string {
  const trimmed = icon.trim();
  if (trimmed.startsWith("/") && !trimmed.startsWith("//")) {
    return trimmed.split("?")[0] ?? trimmed;
  }
  if (trimmed.startsWith("http://") || trimmed.startsWith("https://")) {
    try {
      return new URL(trimmed).pathname;
    } catch {
      return trimmed;
    }
  }
  return trimmed;
}

/**
 * Custom status-page logo URL, or null to use the Phoenix mascot
 * (`/brand/phoenix-mascot.svg` via BrandMark).
 *
 * Empty values and Uptime Kuma's default `/icon.svg` are not custom logos.
 */
export function customStatusPageIcon(
  icon: string | undefined | null,
): string | null {
  const raw = icon?.trim() ?? "";
  if (!raw) return null;
  if (KUMA_DEFAULT_ICON_PATHS.has(pathOf(raw).toLowerCase())) return null;
  return raw;
}
