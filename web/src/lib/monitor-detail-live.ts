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
 * differs from `lastKnownStatus`.
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
  if (history.length > 0 && history[0].status === entry.status) {
    // Same status — refresh the leading row with the latest check.
    return [
      {
        ...history[0],
        ping: entry.ping,
        message: entry.message || history[0].message,
        time: entry.time,
      },
      ...history.slice(1),
    ];
  }
  if (history.length === 0) return [entry];
  return history;
}
