import { describe, expect, test } from "bun:test";
import {
  defaultCheckProbe,
  mergeRecentChecks,
  regionalCheckEvent,
} from "./recent-checks";
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
