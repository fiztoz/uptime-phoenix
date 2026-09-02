/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  DEFAULT_STATUS_ORDER,
  EMPTY_CRITERIA,
  UNGROUPED,
  applyCriteriaToParams,
  collectTags,
  collectTypes,
  criteriaFromParams,
  criteriaToSearchString,
  filterMonitors,
  groupPath,
  groupSubtreeIds,
  hasActiveFilters,
  isGroupBrowseCriteria,
  monitorTags,
  monitorsInGroup,
  normalizeStatusOrder,
  sortDashboardMonitors,
  summarizeGroup,
  tallyMonitors,
  type FilterCriteria,
  type FilterableMonitor,
} from "./monitor-filter";
import type { MonitorGroupView } from "$lib/api/monitorGroups";

function group(
  id: number,
  name: string,
  parent_id: number | null = null,
): MonitorGroupView {
  return {
    id,
    name,
    description: "",
    parent_id,
    condition: "worst_of_children",
    threshold: 0,
    threshold_is_percent: false,
    weight: 0,
    collapsed: false,
    status: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  };
}

function monitor(
  id: number,
  overrides: Partial<FilterableMonitor> = {},
): FilterableMonitor {
  return {
    id,
    name: `monitor-${id}`,
    type: "http",
    status: "up",
    target: `https://svc-${id}.example.com`,
    group_id: null,
    ...overrides,
  };
}

//   1 Edge
//   └── 2 EU        (└── 4 EU-West)
//   3 Internal
const GROUPS: MonitorGroupView[] = [
  group(1, "Edge"),
  group(2, "EU", 1),
  group(3, "Internal"),
  group(4, "EU-West", 2),
];

const criteria = (over: Partial<FilterCriteria> = {}): FilterCriteria => ({
  ...EMPTY_CRITERIA,
  ...over,
});

describe("groupSubtreeIds", () => {
  test("includes the root and every descendant", () => {
    expect([...groupSubtreeIds(GROUPS, 1)].sort()).toEqual([1, 2, 4]);
  });

  test("a leaf group is just itself", () => {
    expect([...groupSubtreeIds(GROUPS, 3)]).toEqual([3]);
  });

  test("survives a parent cycle instead of hanging", () => {
    const cyclic = [group(1, "A", 2), group(2, "B", 1)];
    expect([...groupSubtreeIds(cyclic, 1)].sort()).toEqual([1, 2]);
  });
});

describe("groupPath", () => {
  test("walks from the root down to the group", () => {
    expect(groupPath(GROUPS, 4).map((g) => g.name)).toEqual([
      "Edge",
      "EU",
      "EU-West",
    ]);
  });

  test("unknown group has no path", () => {
    expect(groupPath(GROUPS, 99)).toEqual([]);
  });
});

describe("isGroupBrowseCriteria", () => {
  test("treats a numeric group by itself as hierarchy navigation", () => {
    expect(isGroupBrowseCriteria(criteria({ group: 1 }))).toBe(true);
    expect(isGroupBrowseCriteria(criteria({ group: 1, search: "   " }))).toBe(
      true,
    );
  });

  test("keeps non-group selections and explicit filtering flat", () => {
    expect(isGroupBrowseCriteria(EMPTY_CRITERIA)).toBe(false);
    expect(isGroupBrowseCriteria(criteria({ group: UNGROUPED }))).toBe(false);
    expect(
      isGroupBrowseCriteria(criteria({ group: 1, search: "payments" })),
    ).toBe(false);
    expect(isGroupBrowseCriteria(criteria({ group: 1, tags: ["prod"] }))).toBe(
      false,
    );
    expect(
      isGroupBrowseCriteria(criteria({ group: 1, statuses: ["down"] })),
    ).toBe(false);
    expect(isGroupBrowseCriteria(criteria({ group: 1, type: "http" }))).toBe(
      false,
    );
    expect(
      isGroupBrowseCriteria(criteria({ group: 1, sort: "name-asc" })),
    ).toBe(false);
  });
});

describe("monitorsInGroup", () => {
  test("pulls monitors out of descendant subgroups too", () => {
    const monitors = [
      monitor(1, { group_id: 1 }),
      monitor(2, { group_id: 2 }),
      monitor(3, { group_id: 4 }),
      monitor(4, { group_id: 3 }),
      monitor(5, { group_id: null }),
    ];
    expect(monitorsInGroup(monitors, GROUPS, 1).map((m) => m.id)).toEqual([
      1, 2, 3,
    ]);
  });
});

describe("monitorTags", () => {
  test("no tags field at all (today's backend) yields none", () => {
    expect(monitorTags(monitor(1))).toEqual([]);
  });

  test("reads the embedded {id,name,color,value} shape", () => {
    const tags = monitorTags({
      tags: [{ id: 7, name: "prod", color: "#ff0000", value: "eu" }],
    });
    expect(tags).toEqual([
      { id: 7, name: "prod", color: "#ff0000", value: "eu" },
    ]);
  });

  test("tolerates the legacy bare-name shape", () => {
    expect(monitorTags({ tags: ["prod", "edge"] }).map((t) => t.name)).toEqual([
      "prod",
      "edge",
    ]);
  });

  test("tolerates the join-row shape with a nested tag", () => {
    const tags = monitorTags({
      tags: [
        { tag_id: 9, value: "v", tag: { id: 9, name: "db", color: "#00f" } },
      ],
    });
    expect(tags).toEqual([{ id: 9, name: "db", color: "#00f", value: "v" }]);
  });

  test("drops malformed entries rather than throwing", () => {
    expect(
      monitorTags({ tags: [null, 42, {}, { name: "" }, "ok"] }).map(
        (t) => t.name,
      ),
    ).toEqual(["ok"]);
    expect(monitorTags({ tags: "nonsense" })).toEqual([]);
  });
});

describe("collectTags / collectTypes", () => {
  test("distinct tags, sorted, colors backfilled", () => {
    const monitors = [
      monitor(1, { tags: [{ id: 1, name: "prod" }] }),
      monitor(2, {
        tags: [
          { id: 1, name: "prod", color: "#f00" },
          { id: 2, name: "api" },
        ],
      }),
    ];
    expect(collectTags(monitors)).toEqual([
      { id: 2, name: "api", color: "", value: "" },
      { id: 1, name: "prod", color: "#f00", value: "" },
    ]);
  });

  test("distinct types actually present", () => {
    const monitors = [
      monitor(1, { type: "http" }),
      monitor(2, { type: "tcp" }),
      monitor(3, { type: "http" }),
    ];
    expect(collectTypes(monitors)).toEqual(["http", "tcp"]);
  });
});

describe("filterMonitors", () => {
  const monitors = [
    monitor(1, {
      name: "Checkout API",
      group_id: 1,
      type: "http",
      status: "up",
    }),
    monitor(2, {
      name: "Payments DB",
      group_id: 2,
      type: "postgres",
      status: "down",
      target: "db.internal:5432",
      tags: [{ id: 1, name: "prod", color: "#f00", value: "" }],
    }),
    monitor(3, {
      name: "edge-cache",
      group_id: 4,
      type: "http",
      status: "paused",
    }),
    monitor(4, {
      name: "Legacy cron",
      group_id: null,
      type: "push",
      status: "pending",
    }),
  ];

  test("no criteria returns everything untouched", () => {
    expect(filterMonitors(monitors, EMPTY_CRITERIA, GROUPS)).toHaveLength(4);
    expect(hasActiveFilters(EMPTY_CRITERIA)).toBe(false);
  });

  test("search matches monitor name, case-insensitively, as a substring", () => {
    const out = filterMonitors(monitors, criteria({ search: "CHECK" }), GROUPS);
    expect(out.map((m) => m.id)).toEqual([1]);
  });

  test("search also matches the target/URL", () => {
    const out = filterMonitors(
      monitors,
      criteria({ search: "db.internal" }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([2]);
  });

  test("search ignores surrounding whitespace", () => {
    expect(
      filterMonitors(monitors, criteria({ search: "  cron  " }), GROUPS),
    ).toHaveLength(1);
  });

  test("group filter includes descendant subgroups", () => {
    const out = filterMonitors(monitors, criteria({ group: 1 }), GROUPS);
    expect(out.map((m) => m.id)).toEqual([1, 2, 3]);
  });

  test("ungrouped sentinel selects only monitors with no group", () => {
    const out = filterMonitors(
      monitors,
      criteria({ group: UNGROUPED }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([4]);
  });

  test("tag filter matches by name, case-insensitively", () => {
    const out = filterMonitors(monitors, criteria({ tags: ["PROD"] }), GROUPS);
    expect(out.map((m) => m.id)).toEqual([2]);
  });

  test("multi-tag filter matches ANY tag (OR within facet)", () => {
    // monitor-2 has tag "prod", monitor-1 has no tags → only 2 matches
    const out = filterMonitors(
      monitors,
      criteria({ tags: ["prod", "nonexistent"] }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([2]);
  });

  test("status and type filters", () => {
    expect(
      filterMonitors(monitors, criteria({ statuses: ["down"] }), GROUPS).map(
        (m) => m.id,
      ),
    ).toEqual([2]);
    expect(
      filterMonitors(monitors, criteria({ statuses: ["paused"] }), GROUPS).map(
        (m) => m.id,
      ),
    ).toEqual([3]);
    expect(
      filterMonitors(monitors, criteria({ type: "http" }), GROUPS).map(
        (m) => m.id,
      ),
    ).toEqual([1, 3]);
  });

  test("multi-status filter matches ANY status (OR within facet)", () => {
    const out = filterMonitors(
      monitors,
      criteria({ statuses: ["up", "paused"] }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([1, 3]);
  });

  test("criteria compose (AND, not OR)", () => {
    const out = filterMonitors(
      monitors,
      criteria({ group: 1, type: "http", statuses: ["up"] }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([1]);
  });

  test("a non-matching combination yields nothing", () => {
    expect(
      filterMonitors(
        monitors,
        criteria({ group: 3, search: "checkout" }),
        GROUPS,
      ),
    ).toEqual([]);
  });

  test("multi-status across facets is AND (status OR tag AND across)", () => {
    // monitor-2 is "down" and has tag "prod"; monitor-1 is "up" and has no tags
    const out = filterMonitors(
      monitors,
      criteria({ statuses: ["down"], tags: ["prod"] }),
      GROUPS,
    );
    expect(out.map((m) => m.id)).toEqual([2]);
  });
});

describe("tallyMonitors / summarizeGroup", () => {
  test("maintenance and paused both count as idle", () => {
    const t = tallyMonitors([
      monitor(1, { status: "up" }),
      monitor(2, { status: "up" }),
      monitor(3, { status: "down" }),
      monitor(4, { status: "pending" }),
      monitor(5, { status: "paused" }),
      monitor(6, { status: "maintenance" }),
    ]);
    expect(t).toEqual({ total: 6, up: 2, down: 1, pending: 1, idle: 2 });
  });

  test("group summary counts monitors recursively but subgroups directly", () => {
    const monitors = [
      monitor(1, { group_id: 1, status: "up" }),
      monitor(2, { group_id: 2, status: "down" }),
      monitor(3, { group_id: 4, status: "paused" }),
      monitor(4, { group_id: 3, status: "up" }),
    ];
    // Group 1 (Edge) holds monitor 1 directly, 2 via EU, 3 via EU-West;
    // only EU (2) is a DIRECT subgroup.
    expect(summarizeGroup(GROUPS, monitors, 1)).toEqual({
      total: 3,
      up: 1,
      down: 1,
      pending: 0,
      idle: 1,
      subgroups: 1,
    });
  });

  test("an empty group summarizes to zeroes", () => {
    expect(summarizeGroup(GROUPS, [], 3)).toEqual({
      total: 0,
      up: 0,
      down: 0,
      pending: 0,
      idle: 0,
      subgroups: 0,
    });
  });
});

describe("sortDashboardMonitors", () => {
  const monitors = [
    monitor(1, { name: "Bravo", status: "up" }),
    monitor(2, { name: "Alpha", status: "down" }),
    monitor(3, { name: "Charlie", status: "pending" }),
    monitor(4, { name: "Delta", status: "down" }),
    monitor(5, { name: "Echo", status: "paused" }),
  ];

  test("default preserves the deterministic incoming order", () => {
    expect(
      sortDashboardMonitors(monitors, "default", DEFAULT_STATUS_ORDER).map(
        (m) => m.id,
      ),
    ).toEqual([1, 2, 3, 4, 5]);
  });

  test("status order is fully operator-configurable and stable within a status", () => {
    const order = ["up", "pending", "down", "paused", "maintenance"] as const;
    expect(
      sortDashboardMonitors(monitors, "status", order).map((m) => m.id),
    ).toEqual([1, 3, 2, 4, 5]);
  });

  test("normalizes duplicates, invalid values, and omitted statuses", () => {
    expect(normalizeStatusOrder(["up", "up", "exploded", "down"])).toEqual([
      "up",
      "down",
      "pending",
      "maintenance",
      "paused",
    ]);
  });

  test("sorts names in either direction", () => {
    expect(
      sortDashboardMonitors(monitors, "name-asc", DEFAULT_STATUS_ORDER).map(
        (m) => m.name,
      ),
    ).toEqual(["Alpha", "Bravo", "Charlie", "Delta", "Echo"]);
    expect(
      sortDashboardMonitors(monitors, "name-desc", DEFAULT_STATUS_ORDER).map(
        (m) => m.name,
      ),
    ).toEqual(["Echo", "Delta", "Charlie", "Bravo", "Alpha"]);
  });

  test("sorts response times while keeping missing measurements last", () => {
    const pings = new Map([
      [1, 80],
      [2, 20],
      [3, 0],
      [4, 120],
    ]);
    const responseTime = (m: FilterableMonitor) => pings.get(m.id);
    expect(
      sortDashboardMonitors(
        monitors,
        "response-asc",
        DEFAULT_STATUS_ORDER,
        responseTime,
      ).map((m) => m.id),
    ).toEqual([2, 1, 4, 3, 5]);
    expect(
      sortDashboardMonitors(
        monitors,
        "response-desc",
        DEFAULT_STATUS_ORDER,
        responseTime,
      ).map((m) => m.id),
    ).toEqual([4, 1, 2, 3, 5]);
  });
});

describe("URL codec", () => {
  test("round-trips every criterion", () => {
    const c = criteria({
      search: "api",
      group: 12,
      tags: ["prod", "edge"],
      statuses: ["down", "pending"],
      type: "http",
      sort: "status",
      statusOrder: ["up", "pending", "down", "maintenance", "paused"],
    });
    const qs = criteriaToSearchString(new URLSearchParams(), c);
    expect(criteriaFromParams(new URLSearchParams(qs))).toEqual(c);
  });

  test("round-trips the ungrouped sentinel", () => {
    const c = criteria({ group: UNGROUPED });
    const qs = criteriaToSearchString(new URLSearchParams(), c);
    expect(qs).toBe("?group=none");
    expect(criteriaFromParams(new URLSearchParams(qs)).group).toBe(UNGROUPED);
  });

  test("no active filter produces an empty query string", () => {
    expect(criteriaToSearchString(new URLSearchParams(), EMPTY_CRITERIA)).toBe(
      "",
    );
  });

  test("empty params parse to the empty criteria", () => {
    expect(criteriaFromParams(new URLSearchParams())).toEqual(EMPTY_CRITERIA);
  });

  test("rejects a garbage status instead of filtering on it", () => {
    expect(
      criteriaFromParams(new URLSearchParams("?status=exploded")).statuses,
    ).toEqual([]);
    expect(
      criteriaFromParams(new URLSearchParams("?statuses=exploded")).statuses,
    ).toEqual([]);
  });

  test("rejects a non-numeric / non-positive group id", () => {
    expect(
      criteriaFromParams(new URLSearchParams("?group=abc")).group,
    ).toBeNull();
    expect(
      criteriaFromParams(new URLSearchParams("?group=-3")).group,
    ).toBeNull();
    expect(
      criteriaFromParams(new URLSearchParams("?group=1.5")).group,
    ).toBeNull();
  });

  test("preserves unrelated query params and drops cleared filters", () => {
    const params = new URLSearchParams("?tab=grid&q=old&statuses=up");
    const next = applyCriteriaToParams(params, criteria({ search: "new" }));
    expect(next.get("tab")).toBe("grid");
    expect(next.get("q")).toBe("new");
    expect(next.has("statuses")).toBe(false);
  });

  test("search is trimmed on the way into the URL", () => {
    const qs = criteriaToSearchString(
      new URLSearchParams(),
      criteria({ search: "  api  " }),
    );
    expect(qs).toBe("?q=api");
  });

  test("backward-compat: old ?status=X is parsed as a one-element statuses array", () => {
    const c = criteriaFromParams(new URLSearchParams("?status=down"));
    expect(c.statuses).toEqual(["down"]);
  });

  test("backward-compat: old ?tag=X is parsed as a one-element tags array", () => {
    const c = criteriaFromParams(new URLSearchParams("?tag=prod"));
    expect(c.tags).toEqual(["prod"]);
  });

  test("new format: multi-status round-trip", () => {
    const c = criteria({ statuses: ["up", "down", "paused"] });
    const qs = criteriaToSearchString(new URLSearchParams(), c);
    expect(qs).toContain("statuses=up%2Cdown%2Cpaused");
    expect(criteriaFromParams(new URLSearchParams(qs))).toEqual(c);
  });

  test("new format: multi-tag round-trip", () => {
    const c = criteria({ tags: ["prod", "edge"] });
    const qs = criteriaToSearchString(new URLSearchParams(), c);
    expect(qs).toContain("tags=prod%2Cedge");
    expect(criteriaFromParams(new URLSearchParams(qs))).toEqual(c);
  });

  test("mixed statuses with invalid entries are silently dropped", () => {
    const c = criteriaFromParams(
      new URLSearchParams("?statuses=up,exploded,down"),
    );
    expect(c.statuses).toEqual(["up", "down"]);
  });

  test("custom status sort order survives a shareable URL", () => {
    const c = criteria({
      sort: "status",
      statusOrder: ["up", "pending", "down", "paused", "maintenance"],
    });
    const qs = criteriaToSearchString(new URLSearchParams(), c);
    expect(qs).toContain("sort=status");
    expect(qs).toContain(
      "status_order=up%2Cpending%2Cdown%2Cpaused%2Cmaintenance",
    );
    expect(criteriaFromParams(new URLSearchParams(qs))).toEqual(c);

    const switchedSort = { ...c, sort: "name-asc" as const };
    const switchedQs = criteriaToSearchString(
      new URLSearchParams(),
      switchedSort,
    );
    expect(criteriaFromParams(new URLSearchParams(switchedQs))).toEqual(
      switchedSort,
    );
  });

  test("rejects an invalid sort and normalizes malformed status order", () => {
    const c = criteriaFromParams(
      new URLSearchParams("?sort=sideways&status_order=up,up,broken,down"),
    );
    expect(c.sort).toBe("default");
    expect(c.statusOrder).toEqual([
      "up",
      "down",
      "pending",
      "maintenance",
      "paused",
    ]);
  });
});
