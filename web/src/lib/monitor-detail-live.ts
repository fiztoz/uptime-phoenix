/**
 * Pure helpers that merge one live WS heartbeat into the monitor detail
 * page's Status History and timeline streams. Unit-tested in isolation.
 */
import type { Heartbeat } from "#lib/monitor-types.js";

/** Status History table keeps at most this many transition rows. */
const STATUS_HISTORY_MAX = 100;
/** The timeline strip keeps at most this many segments. */
const TIMELINE_MAX = 60;

export interface LiveMergeOptions {
  /** The stream's last API load failed: its emptiness is not "no data". */
  loadFailed?: boolean;
}

/**
 * Merge a live heartbeat into the Status History rows (newest first). The
 * table lists status *changes*: a new row is prepended only when the status
 * differs from `lastKnownStatus`; an empty table is seeded with the beat.
 */
export function mergeLiveStatusHistory(
  history: Heartbeat[],
  entry: Heartbeat,
  lastKnownStatus: string | null,
  options: LiveMergeOptions = {},
): Heartbeat[] {
  void options;
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

/**
 * Merge a live heartbeat into the timeline strip (oldest first): append it
 * when it is newer than the latest segment; an empty strip is seeded with it.
 */
export function mergeLiveTimeline(
  timeline: Heartbeat[],
  entry: Heartbeat,
  options: LiveMergeOptions = {},
): Heartbeat[] {
  void options;
  if (timeline.length === 0) return [entry];
  const latest = timeline[timeline.length - 1];
  if (Date.parse(entry.time) > Date.parse(latest.time))
    return [...timeline, entry].slice(-TIMELINE_MAX);
  return timeline;
}
