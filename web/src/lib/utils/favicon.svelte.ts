/**
 * Canvas-based favicon badge when monitors are down.
 * Restores the default favicon when downCount is zero.
 */

const FAVICON_SIZE = 32;
const BADGE_RADIUS = 7;
const DEFAULT_FAVICON = "/favicon.svg";

let originalHref: string | null = null;
let baseImage: HTMLImageElement | null = null;
let baseImageLoaded = false;

function getIconLink(): HTMLLinkElement | null {
  if (typeof document === "undefined") return null;
  return document.querySelector<HTMLLinkElement>('link[rel="icon"]');
}

function ensureOriginalHref(): void {
  const link = getIconLink();
  if (!link) return;
  if (originalHref === null) {
    originalHref = link.getAttribute("href") || DEFAULT_FAVICON;
  }
}

function loadBaseImage(): Promise<void> {
  if (baseImageLoaded && baseImage) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = () => {
      baseImage = img;
      baseImageLoaded = true;
      resolve();
    };
    img.onerror = () => reject(new Error("Failed to load favicon"));
    img.src = originalHref || DEFAULT_FAVICON;
  });
}

/**
 * Update the tab favicon: red corner badge when downCount > 0, otherwise default.
 */
export async function updateFaviconBadge(downCount: number): Promise<void> {
  if (typeof document === "undefined") return;

  ensureOriginalHref();
  const link = getIconLink();
  if (!link) return;

  if (downCount <= 0) {
    link.href = originalHref || DEFAULT_FAVICON;
    return;
  }

  try {
    await loadBaseImage();
  } catch {
    // Fallback: simple red dot SVG data URL
    link.href = badgeSvgDataUrl(downCount);
    return;
  }

  const canvas = document.createElement("canvas");
  canvas.width = FAVICON_SIZE;
  canvas.height = FAVICON_SIZE;
  const ctx = canvas.getContext("2d");
  if (!ctx || !baseImage) return;

  ctx.drawImage(baseImage, 0, 0, FAVICON_SIZE, FAVICON_SIZE);

  ctx.fillStyle = "#dc2626";
  ctx.beginPath();
  ctx.arc(
    FAVICON_SIZE - BADGE_RADIUS,
    BADGE_RADIUS,
    BADGE_RADIUS,
    0,
    Math.PI * 2,
  );
  ctx.fill();
  ctx.strokeStyle = "#ffffff";
  ctx.lineWidth = 1.5;
  ctx.stroke();

  link.href = canvas.toDataURL("image/png");
}

function badgeSvgDataUrl(downCount: number): string {
  const label = downCount > 9 ? "9+" : String(downCount);
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="32" height="32" viewBox="0 0 32 32">
  <circle cx="24" cy="8" r="8" fill="#dc2626" stroke="#fff" stroke-width="1.5"/>
  <text x="24" y="11" text-anchor="middle" fill="#fff" font-size="9" font-family="system-ui,sans-serif" font-weight="bold">${label}</text>
</svg>`;
  return `data:image/svg+xml,${encodeURIComponent(svg)}`;
}
