/**
 * Last-known monitor list for the dashboard's first paint.
 *
 * WebSocket monitor.list waits on a batched heartbeat lookup. When that is
 * slow, the admin shell used to sit on skeleton cards until the socket
 * finished (or canceled). Restoring the previous snapshot lets the grid
 * render immediately; REST and WS overwrite it when they arrive.
 *
 * Keyed by a JWT fingerprint so a later login on the same tab cannot see
 * the previous user's monitors.
 */

export const MONITOR_SNAPSHOT_CACHE_KEY = "phoenix:monitor-snapshot";

type CachePayload = {
  token: string;
  monitors: unknown;
};

function defaultStorage(): Pick<
  Storage,
  "getItem" | "setItem" | "removeItem"
> | null {
  if (typeof sessionStorage === "undefined") return null;
  return sessionStorage;
}

/** Trailing slice of the JWT — enough to distinguish sessions, not the secret. */
export function tokenFingerprint(token: string): string {
  return token.slice(-16);
}

function isMonitorRow(value: unknown): value is { id: number } {
  if (!value || typeof value !== "object") return false;
  const id = (value as { id?: unknown }).id;
  return typeof id === "number" && Number.isFinite(id) && id > 0;
}

export function readMonitorSnapshotCache<T extends { id: number }>(
  token: string | null,
  storage: Pick<Storage, "getItem"> | null = defaultStorage(),
): T[] | null {
  if (!token || !storage) return null;
  try {
    const raw = storage.getItem(MONITOR_SNAPSHOT_CACHE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as CachePayload;
    if (parsed.token !== tokenFingerprint(token)) return null;
    if (!Array.isArray(parsed.monitors)) return null;
    const rows = parsed.monitors.filter(isMonitorRow);
    return rows as T[];
  } catch {
    return null;
  }
}

export function writeMonitorSnapshotCache(
  token: string | null,
  monitors: unknown[],
  storage: Pick<Storage, "setItem"> | null = defaultStorage(),
): void {
  if (!token || !storage) return;
  try {
    const payload: CachePayload = {
      token: tokenFingerprint(token),
      monitors,
    };
    storage.setItem(MONITOR_SNAPSHOT_CACHE_KEY, JSON.stringify(payload));
  } catch {
    // Quota / private mode — first paint just waits on REST/WS.
  }
}

export function clearMonitorSnapshotCache(
  storage: Pick<Storage, "removeItem"> | null = defaultStorage(),
): void {
  if (!storage) return;
  try {
    storage.removeItem(MONITOR_SNAPSHOT_CACHE_KEY);
  } catch {
    // Ignore.
  }
}
