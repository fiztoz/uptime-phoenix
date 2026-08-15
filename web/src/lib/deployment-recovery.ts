const PRELOAD_RELOAD_KEY = "phoenix:last-preload-reload";

export const PRELOAD_RELOAD_COOLDOWN_MS = 30_000;

interface PreloadErrorEvent {
  preventDefault(): void;
}

interface RecoveryOptions {
  storage: Pick<Storage, "getItem" | "setItem">;
  reload(): void;
  now?: () => number;
}

/**
 * Reloads an open SPA once when a rolling deployment removes a chunk that its
 * old route manifest still references. The cooldown prevents a bad rollout
 * from trapping the browser in a hard-refresh loop.
 */
export function recoverFromPreloadError(
  event: PreloadErrorEvent,
  { storage, reload, now = Date.now }: RecoveryOptions,
): boolean {
  const currentTime = now();
  let lastReload = 0;

  try {
    lastReload = Number(storage.getItem(PRELOAD_RELOAD_KEY)) || 0;
  } catch {
    // Storage can be unavailable in privacy-restricted browsing contexts.
  }

  if (lastReload > 0 && currentTime - lastReload < PRELOAD_RELOAD_COOLDOWN_MS) {
    return false;
  }

  event.preventDefault();
  try {
    storage.setItem(PRELOAD_RELOAD_KEY, String(currentTime));
  } catch {
    // A reload still gives the browser a chance to pick up the new manifest.
  }
  reload();
  return true;
}
