/**
 * Pure helpers that merge one live WS heartbeat into the monitor detail
 * page's Status History and timeline streams. Unit-tested in isolation.
 */
import type { Heartbeat } from "#lib/monitor-types.js";

/** Status History table keeps at most this many transition rows. */
const STATUS_HISTORY_MAX = 100;

/**
 * Merge a live heartbeat into the Status History rows (newest first). The
 * table lists status *changes*: a new row is prepended only when the status
 * differs from `lastKnownStatus`; an empty table is seeded with the beat.
 */
export function mergeLiveStatusHistory(
  history: Heartbeat[],
  entry: Heartbeat,
  lastKnownStatus: string | null,
): Heartbeat[] {
  if (lastKnownStatus !== null && lastKnownStatus !== entry.status) {
    // Status transition — prepend a new history row.
    if (history.some((h) => h.time === entry.time && h.status === entry.status))
      return history;
    return [entry, ...history].slice(0, STATUS_HISTORY_MAX);
  }
  // Same status: the leading row is the transition that started this status.
  // Its time and message describe that change, not the latest check, so a
  // live beat leaves it as-is (GitHub #197). Overwriting it also fought the
  // server row that refreshOverallHistory puts back.
  if (history.length === 0) return [entry];
  return history;
}
