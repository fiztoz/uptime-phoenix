/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { chartTimeDomain, sparklinePoints } from "./chart";
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
});

describe("chartTimeDomain", () => {
  test("keeps the selected 24 hour window when the monitor is new", () => {
    const now = new Date("2026-07-26T00:20:00Z");
    const [start, end] = chartTimeDomain(24, now);

    expect(start.toISOString()).toBe("2026-07-25T00:20:00.000Z");
    expect(end.toISOString()).toBe("2026-07-26T00:20:00.000Z");
  });
});
