/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  acceptLiveHeartbeat,
  deriveStatusHint,
  heartbeatsAfterClear,
  latestObservedTime,
  monitorDataSource,
  monitorFromApi,
  resolveDisplayedMonitor,
  shouldStopPostClearPolling,
} from "./monitor-detail-state";
import type { Monitor } from "$lib/stores/ws.svelte.ts";

const baseMonitor: Monitor = {
  id: 1,
  name: "API",
  type: "push",
  status: "pending",
  active: true,
  config: {},
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

describe("deriveStatusHint", () => {
  test("prefers latest history down over stats ping", () => {
    const hint = deriveStatusHint(
      { status: "down", time: "2026-01-01T01:00:00Z" },
      { status: "up", time: "2026-01-01T02:00:00Z", ping: 10 },
      { current_ping_ms: 50 },
    );
    expect(hint).toBe("down");
  });

  test("uses live status when history absent", () => {
    const hint = deriveStatusHint(
      null,
      {
        status: "down",
        time: "2026-01-01T02:00:00Z",
        ping: 0,
      },
      null,
    );
    expect(hint).toBe("down");
  });

  test("falls back to up only when stats has positive ping and no history/live", () => {
    const hint = deriveStatusHint(null, null, { current_ping_ms: 42 });
    expect(hint).toBe("up");
  });

  test("ignores zero ping stats fallback", () => {
    const hint = deriveStatusHint(null, null, { current_ping_ms: 0 });
    expect(hint).toBeNull();
  });

  test("returns null when no signals", () => {
    expect(deriveStatusHint(null, null, null)).toBeNull();
  });

  test("passes maintenance status through unchanged", () => {
    const hint = deriveStatusHint(
      { status: "maintenance", time: "2026-01-01T01:00:00Z" },
      null,
      null,
    );
    expect(hint).toBe("maintenance");
  });
});

describe("monitorFromApi", () => {
  test("down hint on inactive false monitor", () => {
    const m = monitorFromApi(baseMonitor, "down");
    expect(m.status).toBe("down");
  });

  test("paused when active false regardless of hint", () => {
    const m = monitorFromApi({ ...baseMonitor, active: false }, "down");
    expect(m.status).toBe("paused");
  });

  test("pending when no hint and no status on raw", () => {
    const m = monitorFromApi({
      ...baseMonitor,
      status: undefined as unknown as Monitor["status"],
    });
    expect(m.status).toBe("pending");
  });

  test("maintenance hint on active monitor stays maintenance", () => {
    const m = monitorFromApi(baseMonitor, "maintenance");
    expect(m.status).toBe("maintenance");
  });

  test("paused still wins over a maintenance hint", () => {
    const m = monitorFromApi({ ...baseMonitor, active: false }, "maintenance");
    expect(m.status).toBe("paused");
  });
});

describe("resolveDisplayedMonitor", () => {
  test("prefers live over fetched", () => {
    const live = { ...baseMonitor, status: "up" as const };
    const fetched = { ...baseMonitor, status: "down" as const };
    expect(resolveDisplayedMonitor(live, fetched)?.status).toBe("up");
  });

  test("uses fetched when live missing", () => {
    const fetched = { ...baseMonitor, status: "down" as const };
    expect(resolveDisplayedMonitor(null, fetched)?.status).toBe("down");
  });

  test("keeps fetched interval when live omits scheduling fields", () => {
    const live = { ...baseMonitor, status: "up" as const };
    const fetched = {
      ...baseMonitor,
      status: "down" as const,
      interval: 10,
      timeout: 15,
    };
    const resolved = resolveDisplayedMonitor(live, fetched);
    expect(resolved?.status).toBe("up");
    expect(resolved?.interval).toBe(10);
    expect(resolved?.timeout).toBe(15);
  });

  test("prefers live interval when present", () => {
    const live = { ...baseMonitor, status: "up" as const, interval: 20 };
    const fetched = { ...baseMonitor, interval: 10 };
    expect(resolveDisplayedMonitor(live, fetched)?.interval).toBe(20);
  });
});

describe("latestObservedTime", () => {
  test("returns newest ISO timestamp", () => {
    expect(
      latestObservedTime([
        { time: "2026-01-01T10:00:00Z" },
        { time: "2026-01-01T12:00:00Z" },
        { time: "2026-01-01T11:00:00Z" },
      ]),
    ).toBe("2026-01-01T12:00:00Z");
  });

  test("returns null for empty input", () => {
    expect(latestObservedTime([])).toBeNull();
  });
});

describe("heartbeatsAfterClear", () => {
  const clearedAt = "2026-01-01T12:00:00Z";

  test("drops rows at or before clearedAt", () => {
    const rows = heartbeatsAfterClear(
      [
        { time: "2026-01-01T11:59:59Z", status: "down" },
        { time: "2026-01-01T12:00:00Z", status: "down" },
        { time: "2026-01-01T12:00:01Z", status: "up" },
      ],
      clearedAt,
    );
    expect(rows).toHaveLength(1);
    expect(rows[0].time).toBe("2026-01-01T12:00:01Z");
  });

  test("returns all rows when clearedAt null", () => {
    expect(
      heartbeatsAfterClear([{ time: "2026-01-01T12:00:00Z" }], null),
    ).toHaveLength(1);
  });
});

describe("shouldStopPostClearPolling", () => {
  test("stops when timeline has data", () => {
    expect(shouldStopPostClearPolling(1, 0)).toBe(true);
  });

  test("stops when history has data", () => {
    expect(shouldStopPostClearPolling(0, 1)).toBe(true);
  });

  test("continues when both empty", () => {
    expect(shouldStopPostClearPolling(0, 0)).toBe(false);
  });
});

describe("monitorDataSource", () => {
  test("prefers ws when monitor is in realtime store", () => {
    expect(monitorDataSource(1, [{ id: 1 }], baseMonitor)).toBe("ws");
  });

  test("uses api when ws store empty but fetched present", () => {
    expect(monitorDataSource(1, [], baseMonitor)).toBe("api");
  });

  test("loading when neither source ready", () => {
    expect(monitorDataSource(1, [], null)).toBe("loading");
  });
});

describe("acceptLiveHeartbeat", () => {
  const clearedAt = "2026-01-01T12:00:00Z";

  test("rejects duplicate time", () => {
    expect(
      acceptLiveHeartbeat({
        clearedAt: null,
        hbTime: "2026-01-01T12:01:00Z",
        lastProcessedTime: "2026-01-01T12:01:00Z",
      }),
    ).toBe(false);
  });

  test("rejects heartbeat at or before clearedAt", () => {
    expect(
      acceptLiveHeartbeat({
        clearedAt,
        hbTime: "2026-01-01T12:00:00Z",
        lastProcessedTime: null,
      }),
    ).toBe(false);
    expect(
      acceptLiveHeartbeat({
        clearedAt,
        hbTime: "2026-01-01T11:59:00Z",
        lastProcessedTime: null,
      }),
    ).toBe(false);
  });

  test("accepts heartbeat after clearedAt", () => {
    expect(
      acceptLiveHeartbeat({
        clearedAt,
        hbTime: "2026-01-01T12:00:01Z",
        lastProcessedTime: null,
      }),
    ).toBe(true);
  });

  test("accepts all when clearedAt null", () => {
    expect(
      acceptLiveHeartbeat({
        clearedAt: null,
        hbTime: "2026-01-01T12:00:01Z",
        lastProcessedTime: null,
      }),
    ).toBe(true);
  });
});
