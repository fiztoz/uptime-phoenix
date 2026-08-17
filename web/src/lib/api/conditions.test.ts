import { describe, expect, test } from "bun:test";
import {
  applyConditionSnapshotToMap,
  conditionDrivesDashboardAttention,
  conditionKey,
  conditionNeedsAttention,
  conditionUsagePercent,
  displayedConditionState,
  type MonitorCondition,
} from "./conditions";

function condition(
  overrides: Partial<MonitorCondition> = {},
): MonitorCondition {
  return {
    monitor_id: 1,
    kind: "storage",
    state: "ok",
    used: 20,
    limit: 100,
    percent: 20,
    threshold: 80,
    unit: "bytes",
    resource: "Database size",
    scope: "database",
    source: "test",
    message: "normal",
    observed_at: "2030-01-01T00:00:00Z",
    stale_after: "2030-01-01T00:03:00Z",
    last_success_at: "2030-01-01T00:00:00Z",
    ...overrides,
  };
}

describe("monitor condition freshness", () => {
  test("derives stale after the server-provided freshness deadline", () => {
    const row = condition();
    expect(
      displayedConditionState(row, Date.parse("2030-01-01T00:02:59Z")),
    ).toBe("ok");
    expect(
      displayedConditionState(row, Date.parse("2030-01-01T00:03:00Z")),
    ).toBe("stale");
  });

  test("treats warning, error, and stale as attention without changing availability", () => {
    expect(conditionNeedsAttention(condition({ state: "warning" }), 0)).toBe(
      true,
    );
    expect(conditionNeedsAttention(condition({ state: "error" }), 0)).toBe(
      true,
    );
    expect(
      conditionNeedsAttention(
        condition({ state: "ok", stale_after: "2029-12-31T23:59:59Z" }),
        Date.parse("2030-01-01T00:00:00Z"),
      ),
    ).toBe(true);
    expect(
      conditionNeedsAttention(condition(), Date.parse("2030-01-01T00:01:00Z")),
    ).toBe(false);
  });
});

describe("condition usage percent", () => {
  test("clamps a finite percent onto the meter scale", () => {
    expect(conditionUsagePercent(condition({ percent: 6.6 }))).toBe(6.6);
    expect(conditionUsagePercent(condition({ percent: 140 }))).toBe(100);
    expect(conditionUsagePercent(condition({ percent: -4 }))).toBe(0);
    expect(conditionUsagePercent(condition({ percent: null }))).toBeNull();
  });
});

describe("dashboard condition attention", () => {
  const warning = condition({ state: "warning" });
  const stale = condition({
    state: "ok",
    stale_after: "2029-12-31T23:59:59Z",
  });

  test("active warning and error remain attention", () => {
    expect(
      conditionDrivesDashboardAttention(warning, {
        status: "up",
        active: true,
      }),
    ).toBe(true);
    expect(
      conditionDrivesDashboardAttention(condition({ state: "error" }), {
        status: "up",
        active: true,
      }),
    ).toBe(true);
  });

  test("active stale remains attention", () => {
    expect(
      conditionDrivesDashboardAttention(
        stale,
        {
          status: "up",
          active: true,
        },
        Date.parse("2030-01-01T00:00:00Z"),
      ),
    ).toBe(true);
  });

  test("paused and maintenance are not condition-driven attention", () => {
    expect(
      conditionDrivesDashboardAttention(warning, {
        status: "paused",
        active: false,
      }),
    ).toBe(false);
    expect(
      conditionDrivesDashboardAttention(
        stale,
        {
          status: "maintenance",
          active: true,
        },
        Date.parse("2030-01-01T00:00:00Z"),
      ),
    ).toBe(false);
  });

  test("ordinary down or pending is not decided here", () => {
    expect(
      conditionDrivesDashboardAttention(condition(), {
        status: "down",
        active: true,
      }),
    ).toBe(false);
    expect(
      conditionDrivesDashboardAttention(condition(), {
        status: "pending",
        active: true,
      }),
    ).toBe(false);
  });
});

describe("condition snapshot application", () => {
  test("live update wins over an older snapshot", () => {
    const live = condition({ percent: 90, state: "warning" });
    const current = new Map([[conditionKey(live), live]]);
    const older = [condition({ percent: 20, state: "ok" })];
    expect(applyConditionSnapshotToMap(current, older, 1, 2)).toBeNull();
  });

  test("delete during snapshot keeps the row gone", () => {
    const current = new Map<string, MonitorCondition>();
    const older = [condition({ state: "warning" })];
    expect(applyConditionSnapshotToMap(current, older, 3, 4)).toBeNull();
  });

  test("failed REST read keeps existing conditions", () => {
    const live = condition({ state: "warning" });
    const current = new Map([[conditionKey(live), live]]);
    expect(applyConditionSnapshotToMap(current, null, 1, 1)).toBeNull();
  });

  test("older overlapping snapshot cannot replace a newer one", () => {
    const newer = condition({ percent: 40 });
    const current = new Map([[conditionKey(newer), newer]]);
    expect(
      applyConditionSnapshotToMap(current, [condition({ percent: 10 })], 4, 5),
    ).toBeNull();
  });

  test("per-monitor snapshot preserves other monitors", () => {
    const other = condition({ monitor_id: 2, kind: "session_pool" });
    const current = new Map([[conditionKey(other), other]]);
    const next = applyConditionSnapshotToMap(
      current,
      [condition({ monitor_id: 1, kind: "storage", state: "warning" })],
      1,
      1,
      1,
    );
    expect(next?.get("2:session_pool")).toEqual(other);
    expect(next?.get("1:storage")?.state).toBe("warning");
  });

  test("a successful snapshot is applied even when empty so callers must not resubscribe to seq", () => {
    // Empty array is not null — apply returns a new map. applyConditionSnapshot
    // then increments conditionSeq. A $effect that tracked beginConditionSnapshot
    // would refetch forever (429). Callers must untrack that read.
    const next = applyConditionSnapshotToMap(new Map(), [], 0, 0, 4);
    expect(next).not.toBeNull();
    expect(next?.size).toBe(0);
  });

  test("full snapshot replaces the map when no live event occurred", () => {
    const stale = condition({ monitor_id: 9, state: "warning" });
    const current = new Map([[conditionKey(stale), stale]]);
    const next = applyConditionSnapshotToMap(
      current,
      [condition({ monitor_id: 1, state: "ok" })],
      8,
      8,
    );
    expect(next?.has("9:storage")).toBe(false);
    expect(next?.get("1:storage")?.state).toBe("ok");
  });
});
