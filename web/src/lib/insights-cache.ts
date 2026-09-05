/**
 * Stale-while-revalidate cache for the insights reliability page.
 *
 * The page used to blank into skeleton rows on every visit and on every
 * filter change until GET /api/insights answered. Caching the last rows per
 * (period, type, group) window lets the table paint instantly; a background
 * refresh overwrites the stale rows when it lands.
 *
 * Entries are keyed by the caller-supplied owner fingerprint (a slice of the
 * session JWT, provided by the caller) so a later login on the same browser
 * cannot read the previous user's reliability data, and they expire after a
 * TTL so an ancient cache is discarded rather than painted.
 */
import type { InsightsRow } from "$lib/api/insights";

export const INSIGHTS_CACHE_KEY = "phoenix:insights-cache";

/** Stale rows are a first-paint aid only; the live fetch always follows. */
export const INSIGHTS_CACHE_TTL_MS = 30 * 60 * 1000;

type CacheEntry = {
  at: number;
  rows: InsightsRow[];
};

type CacheShape = {
  owner: string;
  entries: Record<string, CacheEntry>;
};

/** In-session mirror so SPA navigation re-renders without re-parsing JSON. */
let mirror: CacheShape | null = null;
let mirrorLoaded = false;

/** Stable key for one request window. */
export function insightsWindowKey(
  period: string,
  type: string,
  groupId: number | undefined,
): string {
  return `${period}|${type || ""}|${groupId ?? ""}`;
}

function defaultStorage(): Pick<Storage, "getItem" | "setItem"> | null {
  try {
    if (typeof localStorage !== "undefined") return localStorage;
  } catch {
    // Sandboxed/private contexts — cache is simply disabled.
  }
  return null;
}

function loadMirror(
  storage: Pick<Storage, "getItem"> | null,
): CacheShape | null {
  if (mirrorLoaded) return mirror;
  mirrorLoaded = true;
  if (!storage) return null;
  try {
    const raw = storage.getItem(INSIGHTS_CACHE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as CacheShape;
    if (!parsed || typeof parsed !== "object" || !parsed.entries) return null;
    mirror = parsed;
    return mirror;
  } catch {
    return null;
  }
}

export function readInsightsCache(
  owner: string | null,
  period: string,
  type: string,
  groupId: number | undefined,
  storage: Pick<Storage, "getItem"> | null = defaultStorage(),
): InsightsRow[] | null {
  if (!owner) return null;
  const shape = loadMirror(storage);
  if (!shape || shape.owner !== owner) return null;
  const entry = shape.entries[insightsWindowKey(period, type, groupId)];
  if (!entry || !Array.isArray(entry.rows)) return null;
  if (typeof entry.at !== "number" || Date.now() - entry.at > INSIGHTS_CACHE_TTL_MS) {
    return null;
  }
  return entry.rows;
}

export function writeInsightsCache(
  owner: string | null,
  period: string,
  type: string,
  groupId: number | undefined,
  rows: InsightsRow[],
  storage: Pick<Storage, "setItem"> | null = defaultStorage(),
): void {
  if (!owner || !storage) return;
  // A different owner invalidates every cached window wholesale.
  if (!mirror || mirror.owner !== owner) {
    mirror = { owner, entries: {} };
  }
  mirror.entries[insightsWindowKey(period, type, groupId)] = {
    at: Date.now(),
    rows,
  };
  mirrorLoaded = true;
  try {
    storage.setItem(INSIGHTS_CACHE_KEY, JSON.stringify(mirror));
  } catch {
    // Quota / private mode — the in-session mirror still serves navigation.
  }
}

export function clearInsightsCache(
  storage: Pick<Storage, "removeItem"> | null = null,
): void {
  mirror = null;
  mirrorLoaded = false;
  const target =
    storage ??
    (typeof localStorage !== "undefined" ? localStorage : null);
  if (!target) return;
  try {
    target.removeItem(INSIGHTS_CACHE_KEY);
  } catch {
    // Ignore.
  }
}
