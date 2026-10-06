/// <reference types="bun-types" />
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import {
  advanceChartWindowEnd,
  bucketHeartbeats,
  chartTimeDomain,
  latestChartTime,
  sparklinePoints,
} from "./chart";
import type { Heartbeat } from "$lib/api/heartbeats.js";

function heartbeat(
  id: number,
  time: string,
  ping: number,
  status: Heartbeat["status"] = "up",
): Heartbeat {
  return {
    id,
    monitor_id: 1,
    status,
    ping,
    message: "",
    time,
    important: false,
  };
}

describe("sparklinePoints", () => {
  test("sorts descending API history before drawing", () => {
    const points = sparklinePoints([
      heartbeat(3, "2026-07-26T00:03:00Z", 23),
      heartbeat(2, "2026-07-26T00:02:00Z", 22),
      heartbeat(1, "2026-07-26T00:01:00Z", 21),
    ]);

    expect(points.map((point) => point.time)).toEqual([
      Date.parse("2026-07-26T00:01:00Z"),
      Date.parse("2026-07-26T00:02:00Z"),
      Date.parse("2026-07-26T00:03:00Z"),
    ]);
  });

  test("deduplicates a REST row and matching live heartbeat", () => {
    const points = sparklinePoints([
      heartbeat(1, "2026-07-26T00:01:00Z", 20),
      heartbeat(-1, "2026-07-26T00:01:00Z", 21),
    ]);

    expect(points).toHaveLength(1);
    expect(points[0].value).toBe(21);
  });

  test("drops zero ping and invalid timestamps", () => {
    const points = sparklinePoints([
      heartbeat(1, "2026-07-26T00:01:00Z", 0, "down"),
      heartbeat(2, "not-a-date", 20),
      heartbeat(3, "2026-07-26T00:03:00Z", 23),
    ]);

    expect(points).toHaveLength(1);
    expect(points[0].value).toBe(23);
  });

  test("correctly filters out NaN, Infinity, 0, and negative ping values", () => {
    const points = sparklinePoints([
      heartbeat(1, "2026-07-26T00:01:00Z", NaN, "down"),
      heartbeat(2, "2026-07-26T00:02:00Z", Infinity, "down"),
      heartbeat(3, "2026-07-26T00:03:00Z", -Infinity, "down"),
      heartbeat(4, "2026-07-26T00:04:00Z", 0, "down"),
      heartbeat(5, "2026-07-26T00:05:00Z", -15, "down"),
      heartbeat(6, "2026-07-26T00:06:00Z", 42, "up"),
    ]);

    expect(points).toHaveLength(1);
    expect(points[0].value).toBe(42);
    expect(points[0].time).toBe(Date.parse("2026-07-26T00:06:00Z"));
  });
});

describe("bucketHeartbeats", () => {
  test("ignores ping <= 0 when a down heartbeat precedes an up heartbeat in the same bucket", () => {
    const buckets = bucketHeartbeats(
      [
        heartbeat(1, "2026-07-26T00:00:10Z", 0, "down"),
        heartbeat(2, "2026-07-26T00:00:20Z", -1, "down"),
        heartbeat(3, "2026-07-26T00:00:30Z", 50, "up"),
      ],
      60_000,
    );

    expect(buckets).toHaveLength(1);
    expect(buckets[0].min).toBe(50);
    expect(buckets[0].avg).toBe(50);
    expect(buckets[0].max).toBe(50);
    expect(buckets[0].time).toEqual(new Date("2026-07-26T00:00:00Z"));
  });

  test("calculates correct min, avg, max when down and multiple up heartbeats share a bucket", () => {
    const buckets = bucketHeartbeats(
      [
        heartbeat(1, "2026-07-26T00:00:05Z", -1, "down"),
        heartbeat(2, "2026-07-26T00:00:10Z", 100, "up"),
        heartbeat(3, "2026-07-26T00:00:20Z", 30, "up"),
        heartbeat(4, "2026-07-26T00:00:30Z", 0, "down"),
      ],
      60_000,
    );

    expect(buckets).toHaveLength(1);
    expect(buckets[0].min).toBe(30);
    expect(buckets[0].avg).toBe(65);
    expect(buckets[0].max).toBe(100);
  });

  test("returns empty array when all heartbeats in a bucket have ping <= 0", () => {
    const buckets = bucketHeartbeats(
      [
        heartbeat(1, "2026-07-26T00:00:10Z", 0, "down"),
        heartbeat(2, "2026-07-26T00:00:20Z", -5, "down"),
      ],
      60_000,
    );

    expect(buckets).toEqual([]);
  });
});

describe("chartTimeDomain", () => {
  test("keeps the selected 24 hour window when the monitor is new", () => {
    const now = new Date("2026-07-26T00:20:00Z");
    const [start, end] = chartTimeDomain(24, now);

    expect(start.toISOString()).toBe("2026-07-25T00:20:00.000Z");
    expect(end.toISOString()).toBe("2026-07-26T00:20:00.000Z");
  });
});

describe("latestChartTime", () => {
  test("reads the newest bucket and interval boundary", () => {
    const latest = latestChartTime({
      buckets: [
        { time: "2026-07-26T10:00:00Z" },
        { time: "2026-07-26T11:00:00Z" },
      ],
      downtime_intervals: [
        { start: "2026-07-26T09:00:00Z", end: "2026-07-26T12:30:00Z" },
      ],
      unknown_intervals: [
        { start: "2026-07-26T12:00:00Z", end: "2026-07-26T12:45:00Z" },
      ],
    });

    expect(latest).toBe(Date.parse("2026-07-26T12:45:00Z"));
  });

  test("returns null for empty or unparseable payloads", () => {
    expect(latestChartTime(null)).toBeNull();
    expect(latestChartTime({})).toBeNull();
    expect(latestChartTime({ buckets: [{ time: "not-a-date" }] })).toBeNull();
  });
});

describe("advanceChartWindowEnd", () => {
  // GitHub #62: the rolling window must advance with accepted chart responses
  // while the selected range stays unchanged.
  const t12 = Date.parse("2026-07-26T12:00:00Z");
  const t13 = Date.parse("2026-07-26T13:00:00Z");

  test("later chart data advances the domain end and preserves its duration", () => {
    const first = advanceChartWindowEnd(null, t12, {
      buckets: [{ time: "2026-07-26T11:55:00Z" }],
    });
    const [firstStart, firstEnd] = chartTimeDomain(24, new Date(first));

    // Same selected range, one hour later, with a bucket past the old end.
    const second = advanceChartWindowEnd(first, t13, {
      buckets: [{ time: "2026-07-26T12:30:00Z" }],
    });
    const [secondStart, secondEnd] = chartTimeDomain(24, new Date(second));

    expect(secondEnd.getTime()).toBeGreaterThan(firstEnd.getTime());
    expect(secondEnd.getTime() - secondStart.getTime()).toBe(
      24 * 60 * 60 * 1000,
    );
    // The freshly arrived bucket is inside the advanced domain.
    expect(Date.parse("2026-07-26T12:30:00Z")).toBeGreaterThanOrEqual(
      secondStart.getTime(),
    );
    expect(Date.parse("2026-07-26T12:30:00Z")).toBeLessThanOrEqual(
      secondEnd.getTime(),
    );
  });

  test("new DOWN and UNKNOWN intervals stay inside the advanced domain", () => {
    const first = advanceChartWindowEnd(null, t12, {
      buckets: [],
      downtime_intervals: [],
    });
    const second = advanceChartWindowEnd(first, t13, {
      buckets: [],
      downtime_intervals: [
        { start: "2026-07-26T12:10:00Z", end: "2026-07-26T12:40:00Z" },
      ],
      unknown_intervals: [
        { start: "2026-07-26T12:45:00Z", end: "2026-07-26T12:55:00Z" },
      ],
    });
    const [start, end] = chartTimeDomain(24, new Date(second));

    for (const iso of ["2026-07-26T12:10:00Z", "2026-07-26T12:55:00Z"]) {
      expect(Date.parse(iso)).toBeGreaterThanOrEqual(start.getTime());
      expect(Date.parse(iso)).toBeLessThanOrEqual(end.getTime());
    }
  });

  test("stale responses never move the window end backward", () => {
    const first = advanceChartWindowEnd(null, t13, {
      buckets: [{ time: "2026-07-26T12:55:00Z" }],
    });
    // An older response lands afterwards: earlier clock AND earlier data.
    const second = advanceChartWindowEnd(first, t12, {
      buckets: [{ time: "2026-07-26T11:00:00Z" }],
    });

    expect(second).toBe(first);
  });

  test("a server timestamp past a slow client clock is never clipped", () => {
    const end = advanceChartWindowEnd(null, t12, {
      buckets: [{ time: "2026-07-26T12:30:00Z" }],
    });

    expect(end).toBe(Date.parse("2026-07-26T12:30:00Z"));
    const [start, domainEnd] = chartTimeDomain(24, new Date(end));
    expect(Date.parse("2026-07-26T12:30:00Z")).toBeLessThanOrEqual(
      domainEnd.getTime(),
    );
    expect(Date.parse("2026-07-26T12:30:00Z")).toBeGreaterThan(start.getTime());
  });
});

describe("ResponseTimeChart wiring", () => {
  const source = readFileSync(
    join(import.meta.dir, "..", "components", "ResponseTimeChart.svelte"),
    "utf8",
  );

  test("xDomain derives from the tracked window end, not an untracked clock", () => {
    // Regression for GitHub #62: `chartTimeDomain(selectedHours)` froze the
    // axis end at mount because `new Date()` inside is not reactive state.
    expect(source).toContain("advanceChartWindowEnd(");
    expect(source).toMatch(/chartTimeDomain\(selectedHours, new Date\(/);
    expect(source).not.toMatch(/chartTimeDomain\(selectedHours\)/);
  });
});
