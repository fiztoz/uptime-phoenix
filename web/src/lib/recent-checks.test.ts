import { describe, expect, test } from "bun:test";
import {
  defaultCheckProbe,
  mergeRecentChecks,
  regionalCheckEvent,
  timelineLayout,
} from "./recent-checks";
import { mergeLiveTimeline } from "./monitor-detail-live";
import type { Heartbeat } from "./monitor-types";

const beat = (id: number): Heartbeat => ({
  id,
  monitor_id: 1104,
  status: "up",
  ping: 91,
  message: "",
  time: new Date(Date.UTC(2026, 9, 1, 0, id)).toISOString(),
  important: false,
  scope: "regional",
  latency_available: true,
});

describe("recent checks preserve source identity instead of overall interval counts", () => {
  test("defaults to the sole remote region, never pools multi-region checks", () => {
    expect(defaultCheckProbe([{ probe_id: "oregon" }])).toBe("oregon");
    expect(defaultCheckProbe([{ probe_id: "local" }])).toBeNull();
    expect(defaultCheckProbe([])).toBeNull();
    expect(
      defaultCheckProbe([{ probe_id: "oregon" }, { probe_id: "tokyo" }]),
    ).toBeNull();
  });
  test("retains a live check arriving while an older history snapshot loads", () => {
    const merged = mergeRecentChecks([beat(1), beat(2)], [beat(2), beat(3)]);
    expect(merged.map((row) => row.id)).toEqual([1, 2, 3]);
    expect(merged.at(-1)?.ping).toBe(91);
  });
  test("deduplicates replay and keeps the most recent sixty in chronological order", () => {
    const snapshot = Array.from({ length: 60 }, (_, i) => beat(i + 1));
    const rows = mergeRecentChecks(snapshot, [
      beat(62),
      beat(61),
      beat(62),
      beat(1),
    ]);
    expect(rows).toHaveLength(60);
    expect(rows[0].id).toBe(3);
    expect(rows.at(-1)?.id).toBe(62);
  });
  test("another region or monitor cannot add a check to the selected bar", () => {
    const payload = { ...beat(1), probe_id: "oregon" };
    expect(regionalCheckEvent(payload, 1104, "oregon")?.scope).toBe("regional");
    expect(regionalCheckEvent(payload, 1104, "tokyo")).toBeNull();
    expect(regionalCheckEvent(payload, 1103, "oregon")).toBeNull();
    expect(regionalCheckEvent(payload, 1104, null)).toBeNull();
    expect(
      regionalCheckEvent({ ...payload, time: "invalid" }, 1104, "oregon"),
    ).toBeNull();
    expect(
      regionalCheckEvent({ ...payload, status: "invalid" }, 1104, "oregon"),
    ).toBeNull();
  });
});

describe("timeline layout (GitHub #194)", () => {
  const H = 3_600_000;
  const now = Date.UTC(2026, 10, 2, 13, 38);
  const at = (msBeforeNow: number) => ({
    time: new Date(now - msBeforeNow).toISOString(),
  });
  const close = (got: number[], want: number[]) => {
    expect(got).toHaveLength(want.length);
    got.forEach((w, i) => expect(w).toBeCloseTo(want[i], 6));
  };

  test("local fixed-cadence checks keep equal widths", () => {
    const checks = [at(90_000), at(60_000), at(30_000)];
    const layout = timelineLayout(checks, {
      overall: false,
      nowMs: now,
      windowHours: 24,
      truncated: false,
    });
    expect(layout.lead).toBe(0);
    close(layout.weights, [1 / 3, 1 / 3, 1 / 3]);
  });

  test("overall intervals are as wide as they last", () => {
    // 24h window: up for 20h, down for 3h50m, up for the last 10 minutes.
    const intervals = [at(24 * H), at(4 * H), at(H / 6)];
    const layout = timelineLayout(intervals, {
      overall: true,
      nowMs: now,
      windowHours: 24,
      truncated: false,
    });
    expect(layout.lead).toBe(0);
    expect(layout.leadDurationMs).toBe(0);
    close(layout.weights, [20 / 24, (4 - 1 / 6) / 24, 1 / 6 / 24]);
    expect(layout.durationsMs).toEqual([20 * H, (4 - 1 / 6) * H, (1 / 6) * H]);
  });

  test("time before a new monitor's first interval is marked, not stretched", () => {
    // Created 2h ago: one UP interval must cover 2/24 of the axis.
    const layout = timelineLayout([at(2 * H)], {
      overall: true,
      nowMs: now,
      windowHours: 24,
      truncated: false,
    });
    expect(layout.lead).toBeCloseTo(22 / 24, 6);
    expect(layout.leadDurationMs).toBe(22 * H);
    close(layout.weights, [2 / 24]);
    expect(layout.durationsMs).toEqual([2 * H]);
  });

  test("a capped list starts the axis at its oldest interval", () => {
    // The server returned only the newest intervals: the time before them
    // was monitored, so it must not read as a not-monitored lead.
    const layout = timelineLayout([at(2 * H), at(H)], {
      overall: true,
      nowMs: now,
      windowHours: 24,
      truncated: true,
    });
    expect(layout.lead).toBe(0);
    close(layout.weights, [0.5, 0.5]);
  });

  test("an interval older than the window is clipped to it", () => {
    const layout = timelineLayout([at(30 * H), at(12 * H)], {
      overall: true,
      nowMs: now,
      windowHours: 24,
      truncated: false,
    });
    expect(layout.lead).toBe(0);
    close(layout.weights, [0.5, 0.5]);
  });

  test("full 24h window with monitor created inside window and probe count transitions (#194, #223)", () => {
    const windowNow = Date.parse("2026-11-09T02:00:00.000Z");
    // Monitor created at 01:00:00Z inside 24h window.
    // Adjacent segments keep same status ("up") but differ in probe counts (e.g. m14 in Run D).
    // Minute boundary transition from 01:53:00.000Z to 01:58:32.750Z gives 332,750 ms.
    const segments = [
      {
        time: "2026-11-09T01:00:00.000Z",
        status: "up" as const,
        message: "2/2 probes reporting",
      },
      {
        time: "2026-11-09T01:53:00.000Z",
        status: "up" as const,
        message: "1/2 probes reporting",
      },
      {
        time: "2026-11-09T01:58:32.750Z",
        status: "up" as const,
        message: "2/2 probes reporting",
      },
    ];

    const layout = timelineLayout(segments, {
      overall: true,
      nowMs: windowNow,
      windowHours: 24,
      truncated: false,
    });

    const sumDurations = layout.durationsMs.reduce((a, b) => a + b, 0);
    const sumWeights = layout.weights.reduce((a, b) => a + b, 0);

    expect(layout.leadDurationMs).toBeGreaterThan(0);
    expect(layout.lead).toBeGreaterThan(0);
    expect(layout.leadDurationMs).toBe(23 * 3_600_000);
    expect(layout.lead).toBeCloseTo(23 / 24, 6);

    expect(layout.durationsMs).toEqual([3_180_000, 332_750, 87_250]);
    expect(layout.durationsMs[1]).toBe(332_750);
    expect(layout.leadDurationMs + sumDurations).toBe(24 * 3_600_000);
    expect(layout.lead + sumWeights).toBeCloseTo(1, 10);
  });
});

describe("live beats on an overall timeline (GitHub #194)", () => {
  const interval = (status: Heartbeat["status"], time: string): Heartbeat => ({
    ...beat(1),
    id: 1,
    status,
    time,
    ping: 0,
    scope: "overall",
    latency_available: false,
  });

  test("a same-status beat extends the current interval instead of adding one", () => {
    const loaded = [interval("up", "2026-11-02T04:33:00Z")];
    const live = { ...interval("up", "2026-11-02T13:38:00Z"), id: 0 };
    expect(mergeLiveTimeline(loaded, live)).toBe(loaded);
  });

  test("a status change starts a new interval", () => {
    const loaded = [interval("up", "2026-11-02T04:33:00Z")];
    const live = { ...interval("down", "2026-11-02T13:38:00Z"), id: 0 };
    expect(mergeLiveTimeline(loaded, live).map((h) => h.status)).toEqual([
      "up",
      "down",
    ]);
  });
});
