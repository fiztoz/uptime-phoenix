/**
 * Dynamic favicon badge — shows a red dot when any monitor is DOWN.
 * Manipulates the <link rel="icon"> element's href with a canvas overlay.
 */

let canvas: HTMLCanvasElement | null = null;
let baseImage: HTMLImageElement | null = null;
let currentBadge: "none" | "down" = "none";

const FAVICON_HREF = "/brand/phoenix-mascot.png";
const BADGE_COLOR = "#ef4444"; // red-500
const BADGE_RADIUS = 5;

function ensureCanvas() {
  if (!canvas) {
    canvas = document.createElement("canvas");
    canvas.width = 32;
    canvas.height = 32;
  }
}

function loadBaseImage(): Promise<void> {
  return new Promise((resolve) => {
    if (baseImage) {
      resolve();
      return;
    }
    baseImage = new Image();
    baseImage.src = FAVICON_HREF;
    baseImage.onload = () => resolve();
    baseImage.onerror = () => resolve(); // fallback: don't badge
  });
}

function setFavicon(dataUrl: string) {
  let link = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  if (!link) {
    link = document.createElement("link");
    link.rel = "icon";
    document.head.appendChild(link);
  }
  link.href = dataUrl;
}

function drawBadge() {
  ensureCanvas();
  const ctx = canvas!.getContext("2d");
  if (!ctx || !baseImage?.complete) return;

  ctx.clearRect(0, 0, 32, 32);
  ctx.drawImage(baseImage, 0, 0, 32, 32);

  // Draw red circle in bottom-right corner
  ctx.beginPath();
  ctx.arc(
    32 - BADGE_RADIUS - 1,
    32 - BADGE_RADIUS - 1,
    BADGE_RADIUS,
    0,
    Math.PI * 2,
  );
  ctx.fillStyle = BADGE_COLOR;
  ctx.fill();

  // White border
  ctx.strokeStyle = "#171717"; // matches dark background
  ctx.lineWidth = 1.5;
  ctx.stroke();

  setFavicon(canvas!.toDataURL("image/png"));
}

function resetFavicon() {
  setFavicon(FAVICON_HREF);
}

/**
 * Update the favicon badge based on whether any monitors are DOWN.
 * Call this whenever the monitor list changes.
 */
export async function updateFaviconBadge(monitors: Array<{ status: string }>) {
  const hasDown = monitors.some((m) => m.status === "down");

  if (hasDown && currentBadge !== "down") {
    await loadBaseImage();
    drawBadge();
    currentBadge = "down";
  } else if (!hasDown && currentBadge !== "none") {
    resetFavicon();
    currentBadge = "none";
  }
}
