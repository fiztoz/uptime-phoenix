/**
 * Wallboard split: how much vertical space the stats strip takes versus
 * the monitor cards. Persisted so a TV layout survives a refresh.
 */

export const WALLBOARD_STATS_PX_KEY = "phoenix_wallboard_stats_px";
export const WALLBOARD_CARD_PX_KEY = "phoenix_wallboard_card_px";

export const WALLBOARD_STATS_MIN_PX = 72;
export const WALLBOARD_CARDS_MIN_PX = 200;
export const WALLBOARD_SPLITTER_PX = 8;
export const WALLBOARD_STATS_STEP_PX = 16;

/** Grid minmax for one wallboard card. 17rem default matches the previous layout. */
export const WALLBOARD_CARD_DEFAULT_PX = 272;
export const WALLBOARD_CARD_MIN_PX = 192;
export const WALLBOARD_CARD_MAX_PX = 480;
export const WALLBOARD_CARD_STEP_PX = 16;

export function parseWallboardStatsPx(
  raw: string | null | undefined,
): number | null {
  if (raw == null || raw === "") return null;
  const parsed = Number(raw);
  if (!Number.isFinite(parsed) || parsed <= 0) return null;
  return Math.round(parsed);
}

export function clampWallboardStatsPx(px: number, bodyHeight: number): number {
  const max = Math.max(
    WALLBOARD_STATS_MIN_PX,
    bodyHeight - WALLBOARD_CARDS_MIN_PX - WALLBOARD_SPLITTER_PX,
  );
  return Math.min(max, Math.max(WALLBOARD_STATS_MIN_PX, Math.round(px)));
}

export function readWallboardStatsPx(): number | null {
  if (typeof localStorage === "undefined") return null;
  return parseWallboardStatsPx(localStorage.getItem(WALLBOARD_STATS_PX_KEY));
}

export function writeWallboardStatsPx(px: number | null): void {
  if (typeof localStorage === "undefined") return;
  if (px == null) {
    localStorage.removeItem(WALLBOARD_STATS_PX_KEY);
    return;
  }
  localStorage.setItem(WALLBOARD_STATS_PX_KEY, String(Math.round(px)));
}

/** Metric numbers grow with the stats pane; compact default stays text-xl (20px). */
export function wallboardStatFontPx(statsPx: number): number {
  return Math.min(56, Math.max(20, Math.round(statsPx * 0.28)));
}

export function clampWallboardCardPx(px: number): number {
  return Math.min(
    WALLBOARD_CARD_MAX_PX,
    Math.max(WALLBOARD_CARD_MIN_PX, Math.round(px)),
  );
}

export function readWallboardCardPx(): number {
  if (typeof localStorage === "undefined") return WALLBOARD_CARD_DEFAULT_PX;
  return (
    parseWallboardStatsPx(localStorage.getItem(WALLBOARD_CARD_PX_KEY)) ??
    WALLBOARD_CARD_DEFAULT_PX
  );
}

export function writeWallboardCardPx(px: number): void {
  if (typeof localStorage === "undefined") return;
  localStorage.setItem(WALLBOARD_CARD_PX_KEY, String(clampWallboardCardPx(px)));
}

/** 1 = the original 17rem card. */
export function wallboardCardScale(cardPx: number): number {
  return clampWallboardCardPx(cardPx) / WALLBOARD_CARD_DEFAULT_PX;
}
