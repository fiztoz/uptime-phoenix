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
 *
 * The cache lives in localStorage so it survives new tabs and browser
 * restarts — a sessionStorage cache died with the tab, which made every
 * cold load pay the full REST/WS round trip before first paint. Entries
 * carry a TTL so a snapshot older than the window is discarded instead of
 * painted stale.
 */

export const MONITOR_SNAPSHOT_CACHE_KEY = "phoenix:monitor-snapshot";

/**
 * How long a stored snapshot may be used for first paint. Live REST/WS data
 * always overwrites it, so this only bounds how stale an instant paint can
 * be when the backend is unreachable or very slow.
 */
export const MONITOR_SNAPSHOT_TTL_MS = 24 * 60 * 60 * 1000;

type CachePayload = {
  token: string;
  savedAt?: number;
  monitors: unknown;
};

function defaultStorage(): Pick<
  Storage,
  "getItem" | "setItem" | "removeItem"
> | null {
  // localStorage first (persistent across tabs/restarts); sessionStorage as
  // the fallback when localStorage is unavailable (e.g. strict privacy modes
  // or sandboxed iframes where the access itself throws).
  try {
    if (typeof localStorage !== "undefined") return localStorage;
  } catch {
    // Fall through to sessionStorage.
  }
  if (typeof sessionStorage !== "undefined") return sessionStorage;
  return null;
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

function isFresh(savedAt: unknown, now: number): boolean {
  return (
    typeof savedAt === "number" &&
    Number.isFinite(savedAt) &&
    now - savedAt <= MONITOR_SNAPSHOT_TTL_MS
  );
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
    if (!isFresh(parsed.savedAt, Date.now())) return null;
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
      savedAt: Date.now(),
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
  // Also clear sessionStorage when it is not the storage passed in: older
  // builds wrote there, and a logout or token-invalid event must not leave
  // a stale entry behind.
  try {
    if (storage !== sessionStorage && typeof sessionStorage !== "undefined") {
      sessionStorage.removeItem(MONITOR_SNAPSHOT_CACHE_KEY);
    }
  } catch {
    // Ignore.
  }
}
