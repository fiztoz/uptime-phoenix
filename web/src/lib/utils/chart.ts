import type { Heartbeat } from "$lib/api/heartbeats.js";

export interface ChartBucket {
  time: Date;
  min: number;
  avg: number;
  max: number;
}

export interface DowntimeInterval {
  start: Date;
  end: Date;
}

export interface SparklinePoint {
  time: number;
  value: number;
  status: Heartbeat["status"];
}

type SparklineHeartbeat = Pick<Heartbeat, "time" | "ping" | "status">;

/**
 * Convert heartbeat history into a safe SVG path order.
 *
 * The heartbeat list API defaults to newest-first, while live rows are appended
 * to dashboard history. Drawing that array directly makes the path travel
 * backwards through time and then forwards again. Sorting and de-duplicating at
 * this boundary guarantees every chart consumer receives monotonic x values.
 */
export function sparklinePoints(
  heartbeats: SparklineHeartbeat[],
): SparklinePoint[] {
  const byTime = new Map<number, SparklinePoint>();

  for (const heartbeat of heartbeats) {
    const time = new Date(heartbeat.time).getTime();
    if (
      !Number.isFinite(time) ||
      !Number.isFinite(heartbeat.ping) ||
      heartbeat.ping <= 0
    ) {
      continue;
    }
    byTime.set(time, {
      time,
      value: heartbeat.ping,
      status: heartbeat.status,
    });
  }

  return [...byTime.values()].sort((a, b) => a.time - b.time);
}

/** Return the exact time window selected by the user. */
export function chartTimeDomain(hours: number, now = new Date()): [Date, Date] {
  const safeHours = Number.isFinite(hours) && hours > 0 ? hours : 24;
  return [
    new Date(now.getTime() - safeHours * 60 * 60 * 1000),
    new Date(now.getTime()),
  ];
}

/** Payload fields the rolling chart window boundary reads (chart response). */
export interface ChartTimeSource {
  buckets?: Array<{ time: string }>;
  downtime_intervals?: Array<{ start: string; end: string }>;
  unknown_intervals?: Array<{ start: string; end: string }>;
}

/** Newest instant referenced by a chart payload, or null when there is none. */
export function latestChartTime(
  source: ChartTimeSource | null | undefined,
): number | null {
  let latest: number | null = null;
  const take = (value: unknown) => {
    if (typeof value !== "string") return;
    const ms = Date.parse(value);
    if (!Number.isFinite(ms)) return;
    if (latest === null || ms > latest) latest = ms;
  };
  for (const bucket of source?.buckets ?? []) take(bucket.time);
  for (const interval of source?.downtime_intervals ?? []) {
    take(interval.start);
    take(interval.end);
  }
  for (const interval of source?.unknown_intervals ?? []) {
    take(interval.start);
    take(interval.end);
  }
  return latest;
}

/**
 * Rolling window end for the response-time chart (GitHub #62). The end advances
 * alongside accepted chart responses, never moves backward for stale ones, and
 * never lands before the newest datum so freshly arrived buckets and DOWN/
 * UNKNOWN intervals stay inside the domain even when the server clock runs
 * ahead of the browser.
 */
export function advanceChartWindowEnd(
  previousEnd: number | null,
  nowMs: number,
  source: ChartTimeSource | null | undefined,
): number {
  const candidates: number[] = [];
  if (Number.isFinite(nowMs)) candidates.push(nowMs);
  if (previousEnd != null && Number.isFinite(previousEnd)) {
    candidates.push(previousEnd);
  }
  const dataEnd = latestChartTime(source);
  if (dataEnd != null) candidates.push(dataEnd);
  return candidates.length > 0 ? Math.max(...candidates) : 0;
}

/** Group heartbeats into fixed-width buckets with min/avg/max ping. */
export function bucketHeartbeats(
  heartbeats: Heartbeat[],
  bucketMs: number,
): ChartBucket[] {
  if (heartbeats.length === 0 || bucketMs <= 0) return [];

  const sorted = [...heartbeats].sort(
    (a, b) => new Date(a.time).getTime() - new Date(b.time).getTime(),
  );

  const buckets = new Map<
    number,
    { sum: number; count: number; min: number; max: number }
  >();

  for (const hb of sorted) {
    const t = new Date(hb.time).getTime();
    const key = Math.floor(t / bucketMs) * bucketMs;
    let acc = buckets.get(key);
    if (!acc) {
      acc = { sum: 0, count: 0, min: 0, max: 0 };
      buckets.set(key, acc);
    }
    if (Number.isFinite(hb.ping) && hb.ping > 0) {
      acc.sum += hb.ping;
      acc.count++;
      acc.min = acc.count === 1 ? hb.ping : Math.min(acc.min, hb.ping);
      acc.max = acc.count === 1 ? hb.ping : Math.max(acc.max, hb.ping);
    }
  }

  return [...buckets.entries()]
    .sort(([a], [b]) => a - b)
    .filter(([, acc]) => acc.count > 0)
    .map(([key, acc]) => ({
      time: new Date(key),
      min: acc.min,
      avg: acc.sum / acc.count,
      max: acc.max,
    }));
}

/** Detect contiguous down/pending periods for chart markers. */
export function detectDowntimeIntervals(
  heartbeats: Heartbeat[],
): DowntimeInterval[] {
  if (heartbeats.length === 0) return [];

  const sorted = [...heartbeats].sort(
    (a, b) => new Date(a.time).getTime() - new Date(b.time).getTime(),
  );

  const isDown = (s: Heartbeat["status"]) => s === "down" || s === "pending";
  // UNKNOWN breaks a downtime run. It is not uptime; see detectUnknownIntervals.
  const out: DowntimeInterval[] = [];
  let cur: DowntimeInterval | null = null;

  for (const hb of sorted) {
    if (!isDown(hb.status)) {
      cur = null;
      continue;
    }
    const t = new Date(hb.time);
    if (!cur) {
      cur = { start: t, end: t };
      out.push(cur);
    } else {
      cur.end = t;
    }
  }

  return out;
}

/** Pick bucket width from selected chart range (hours). */
export function bucketMsForRange(hours: number): number {
  if (hours <= 1) return 60_000;
  if (hours <= 6) return 5 * 60_000;
  if (hours <= 24) return 15 * 60_000;
  return 60 * 60_000;
}
