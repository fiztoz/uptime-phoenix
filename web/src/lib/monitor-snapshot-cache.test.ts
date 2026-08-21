import { describe, expect, test } from "bun:test";
import {
  clearMonitorSnapshotCache,
  MONITOR_SNAPSHOT_CACHE_KEY,
  readMonitorSnapshotCache,
  tokenFingerprint,
  writeMonitorSnapshotCache,
} from "./monitor-snapshot-cache";

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

describe("monitor snapshot cache", () => {
  test("round-trips monitors for the same token", () => {
    const storage = memoryStorage();
    const token = "header.payload.signature";
    writeMonitorSnapshotCache(token, [{ id: 7, name: "api" }], storage);

    const rows = readMonitorSnapshotCache<{ id: number; name: string }>(
      token,
      storage,
    );
    expect(rows).toEqual([{ id: 7, name: "api" }]);
  });

  test("ignores a snapshot belonging to a different token", () => {
    const storage = memoryStorage();
    writeMonitorSnapshotCache("token-aaaa", [{ id: 1 }], storage);
    expect(readMonitorSnapshotCache("token-bbbb", storage)).toBeNull();
  });

  test("drops rows without a positive numeric id", () => {
    const storage = memoryStorage();
    const token = "jwt";
    writeMonitorSnapshotCache(
      token,
      [{ id: 1 }, { id: "x" }, null, { name: "no-id" }],
      storage,
    );
    expect(readMonitorSnapshotCache(token, storage)).toEqual([{ id: 1 }]);
  });

  test("clear removes the key", () => {
    const storage = memoryStorage();
    writeMonitorSnapshotCache("jwt", [{ id: 1 }], storage);
    clearMonitorSnapshotCache(storage);
    expect(storage.data[MONITOR_SNAPSHOT_CACHE_KEY]).toBeUndefined();
  });

  test("fingerprint is the trailing slice", () => {
    expect(tokenFingerprint("abcdefghijklmnopqrstuvwxyz")).toBe(
      "klmnopqrstuvwxyz",
    );
  });
});
