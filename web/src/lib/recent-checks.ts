import type { Heartbeat, Status } from "#lib/monitor-types.js";

/** Choose an unambiguous measured source; multiple regions keep overall policy. */
export function defaultCheckProbe(
  assignments: Array<{ probe_id: string }>,
): string | null {
  return assignments.length === 1 && assignments[0].probe_id !== "local"
    ? assignments[0].probe_id
    : null;
}

export interface TimelineLayoutOptions {
  /** Segments are overall health intervals, not fixed-cadence checks. */
  overall: boolean;
  /** Right edge of the axis (ms): the newest interval runs until now. */
  nowMs: number;
  /** Requested window, in hours (the strip asks for the last 24h). */
  windowHours: number;
  /** The server capped the list, so older intervals were left out. */
  truncated: boolean;
}

export interface TimelineLayout {
  /** Fraction of the axis before the first segment (not monitored). */
  lead: number;
  /** Duration (ms) before the first segment on an overall time axis. */
  leadDurationMs: number;
  /** Fraction of the axis each segment covers; sums to 1 with `lead`. */
  weights: number[];
  /** Duration (ms) each segment covers on an overall time axis (0 for fixed-cadence checks). */
  durationsMs: number[];
}

/**
 * Width of each strip segment (sorted oldest first). Local and regional
 * checks run on a fixed cadence, so each gets the same share. Overall rows
 * are policy intervals that each start at `time` and run until the next one
 * (the newest until now), so they are laid out on a time axis over the
 * requested window. Time before the first interval of an uncapped list was
 * not monitored (the server starts the stream at the monitor's creation) and
 * becomes `lead`. A capped list starts the axis at its oldest interval, since
 * the time before it was monitored but left out (GitHub #194).
 */
export function timelineLayout(
  segments: Array<{ time: string }>,
  options: TimelineLayoutOptions,
): TimelineLayout {
  const n = segments.length;
  const equal: TimelineLayout = {
    lead: 0,
    leadDurationMs: 0,
    weights: segments.map(() => (n > 0 ? 1 / n : 0)),
    durationsMs: segments.map(() => 0),
  };
  if (!options.overall || n === 0) return equal;
  const starts = segments.map((segment) => Date.parse(segment.time));
  if (starts.some((start) => !Number.isFinite(start))) return equal;
  const end = Math.max(options.nowMs, starts[n - 1]);
  const windowStart = end - options.windowHours * 3_600_000;
  const axisStart = options.truncated
    ? Math.max(starts[0], windowStart)
    : windowStart;
  const total = end - axisStart;
  if (total <= 0) return equal;
  const durationsMs = starts.map((start, i) => {
    const from = Math.max(start, axisStart);
    const to = Math.max(i + 1 < n ? starts[i + 1] : end, axisStart);
    return Math.max(0, to - from);
  });
  const weights = durationsMs.map((duration) => duration / total);
  const leadDurationMs = Math.max(0, Math.min(starts[0], end) - axisStart);
  const lead = leadDurationMs / total;
  return { lead, leadDurationMs, weights, durationsMs };
}

/** Merge checks from one source, retaining newer live rows during a snapshot. */
export function mergeRecentChecks(
  snapshot: Heartbeat[],
  live: Heartbeat[],
  limit = 60,
): Heartbeat[] {
  const rows = new Map(snapshot.map((beat) => [beat.id, beat]));
  for (const beat of live) rows.set(beat.id, beat);
  return [...rows.values()]
    .sort((a, b) => Date.parse(a.time) - Date.parse(b.time) || a.id - b.id)
    .slice(-limit);
}

/** Accept only real regional heartbeat events for the currently selected source. */
export function regionalCheckEvent(
  payload: unknown,
  monitorId: number,
  probeId: string | null,
): Heartbeat | null {
  if (!probeId || typeof payload !== "object" || payload === null) return null;
  const row = payload as Record<string, unknown>;
  const statuses: Status[] = [
    "up",
    "down",
    "pending",
    "maintenance",
    "unknown",
  ];
  if (
    row.monitor_id !== monitorId ||
    row.probe_id !== probeId ||
    typeof row.id !== "number" ||
    !Number.isSafeInteger(row.id) ||
    row.id <= 0 ||
    typeof row.time !== "string" ||
    !Number.isFinite(Date.parse(row.time)) ||
    !statuses.includes(row.status as Status)
  )
    return null;
  return {
    id: row.id,
    monitor_id: monitorId,
    status: row.status as Status,
    time: row.time,
    ping:
      typeof row.ping === "number" && Number.isFinite(row.ping) ? row.ping : 0,
    message: typeof row.message === "string" ? row.message : "",
    important: row.important === true,
    scope: "regional",
    latency_available: true,
  };
}
