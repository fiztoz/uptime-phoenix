/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { bucketHeartbeats, chartTimeDomain, sparklinePoints } from "./chart";
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
