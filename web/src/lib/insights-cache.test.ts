import { describe, expect, test } from "bun:test";
import {
  INSIGHTS_CACHE_KEY,
  INSIGHTS_CACHE_TTL_MS,
  clearInsightsCache,
  insightsWindowKey,
  readInsightsCache,
  writeInsightsCache,
} from "./insights-cache";
import type { InsightsRow } from "$lib/api/insights";

function memoryStorage(initial: Record<string, string> = {}) {
  const data = { ...initial };
  return {
    getItem(key: string) {
      return data[key] ?? null;
    },
    setItem(key: string, value: string) {
      data[key] = value;
    },
    removeItem(key: string) {
      delete data[key];
    },
    data,
  };
}

function row(id: number, name: string): InsightsRow {
  return {
    monitor_id: id,
    monitor_name: name,
    monitor_type: "http",
    group_id: null,
    availability_percent: 99.5,
    outage_count: 1,
    downtime_seconds: 30,
    flap_count: 2,
    latency_avg_ms: 12.5,
    latency_sample_count: 100,
    coverage_percent: 100,
    qualification: "qualified",
  };
}

describe("insights cache", () => {
  test("round-trips rows for the same owner and window", () => {
    const storage = memoryStorage();
    writeInsightsCache("owner-a", "7d", "", undefined, [row(1, "api")], storage);
    const got = readInsightsCache("owner-a", "7d", "", undefined, storage);
    expect(got).toEqual([row(1, "api")]);
  });

  test("windows do not collide", () => {
    const storage = memoryStorage();
    writeInsightsCache("owner-a", "7d", "", undefined, [row(1, "a")], storage);
    expect(readInsightsCache("owner-a", "24h", "", undefined, storage)).toBeNull();
    expect(readInsightsCache("owner-a", "7d", "http", undefined, storage)).toBeNull();
    expect(readInsightsCache("owner-a", "7d", "", 3, storage)).toBeNull();
  });

  test("a different owner cannot read the cache", () => {
    const storage = memoryStorage();
    writeInsightsCache("owner-a", "7d", "", undefined, [row(1, "a")], storage);
    expect(readInsightsCache("owner-b", "7d", "", undefined, storage)).toBeNull();
  });

  test("a different owner replaces the cache wholesale on write", () => {
    const storage = memoryStorage();
    writeInsightsCache("owner-a", "7d", "", undefined, [row(1, "a")], storage);
    writeInsightsCache("owner-b", "7d", "", undefined, [row(2, "b")], storage);
    expect(readInsightsCache("owner-a", "7d", "", undefined, storage)).toBeNull();
    expect(readInsightsCache("owner-b", "7d", "", undefined, storage)).toEqual([
      row(2, "b"),
    ]);
  });

  test("entries older than the TTL are dropped", () => {
    // Reset the in-session mirror so the read goes through the seeded
    // (expired) storage entry rather than a previous test's fresh write.
    clearInsightsCache(memoryStorage());
    const stale = {
      owner: "owner-a",
      entries: {
        [insightsWindowKey("7d", "", undefined)]: {
          at: Date.now() - INSIGHTS_CACHE_TTL_MS - 1000,
          rows: [row(1, "old")],
        },
      },
    };
    const storage = memoryStorage({
      [INSIGHTS_CACHE_KEY]: JSON.stringify(stale),
    });
    expect(readInsightsCache("owner-a", "7d", "", undefined, storage)).toBeNull();
  });

  test("null owner disables the cache", () => {
    const storage = memoryStorage();
    writeInsightsCache(null, "7d", "", undefined, [row(1, "a")], storage);
    expect(storage.data[INSIGHTS_CACHE_KEY]).toBeUndefined();
    expect(readInsightsCache(null, "7d", "", undefined, storage)).toBeNull();
  });

  test("clear removes the key", () => {
    const storage = memoryStorage();
    writeInsightsCache("owner-a", "7d", "", undefined, [row(1, "a")], storage);
    clearInsightsCache(storage);
    expect(storage.data[INSIGHTS_CACHE_KEY]).toBeUndefined();
  });
});
