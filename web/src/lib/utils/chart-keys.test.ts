import { describe, expect, test } from "bun:test";
import { uniqueKeys } from "./chart";

// GitHub #131: the chart API formats interval bounds to the second, so a
// down/up/down flip within one second returns two intervals with the same
// start. Keyed {#each} blocks then threw each_key_duplicate.
const sameSecond = {
  downtime_intervals: [
    { start: "2026-10-28T03:00:00Z", end: "2026-10-28T03:00:00Z" },
    { start: "2026-10-28T03:00:00Z", end: "2026-10-28T03:00:00Z" },
    { start: "2026-10-28T03:05:00Z", end: "2026-10-28T03:06:00Z" },
  ],
};

describe("uniqueKeys", () => {
  test("intervals that share a start second get distinct keys", () => {
    const intervals = sameSecond.downtime_intervals.map((iv) => ({
      start: new Date(iv.start),
      end: new Date(iv.end),
    }));
    const keys = uniqueKeys(intervals, (iv) => iv.start.getTime());
    expect(new Set(keys).size).toBe(intervals.length);
  });

  test("first occurrence keeps its natural key, so keys stay stable", () => {
    expect(uniqueKeys(["a", "b", "a", "a"], (s) => s)).toEqual([
      "a",
      "b",
      "a#1",
      "a#2",
    ]);
  });

  test("suffixed keys cannot collide with a natural key", () => {
    for (const items of [
      ["a", "a", "a#1"],
      ["a", "a#1", "a"],
      ["a#1", "a", "a", "a#1#1", "a"],
    ]) {
      const keys = uniqueKeys(items, (s) => s);
      expect(new Set(keys).size).toBe(items.length);
    }
  });

  test("empty input", () => {
    expect(uniqueKeys([], (x) => String(x))).toEqual([]);
  });
});
