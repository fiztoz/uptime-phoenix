/**
 * Monitor Group ("folder") CRUD API wrappers, plus a faithful TypeScript port
 * of the backend rollup that derives a group's status from its children.
 *
 * API shape follows web/src/lib/api/monitors.ts conventions exactly (see that
 * file). Route/JSON shape is frozen by the shared contract (scratchpad
 * CONTRACT.md) and internal/adapters/http/handlers/monitor_group.go.
 */
import { api } from "./client";
import type { Notification } from "./notifications";

/** Matches internal/core/domain/monitor_group.go GroupCondition. */
export type GroupCondition =
  | "worst_of_children"
  | "all_down"
  | "threshold"
  | "ignore";

/**
 * Options for the condition picker, in the same order as Go's
 * ValidGroupConditions (the order the UI should offer them).
 */
export const GROUP_CONDITIONS: {
  value: GroupCondition;
  label: string;
  help: string;
}[] = [
  {
    value: "worst_of_children",
    label: "Worst of children",
    help: "Any child DOWN trips the whole group DOWN. Default.",
  },
  {
    value: "all_down",
    label: "All children down",
    help: "Only DOWN when every child is DOWN — for redundant pools where one survivor keeps it UP.",
  },
  {
    value: "threshold",
    label: "Threshold",
    help: "DOWN once a configured count (or percent) of children are DOWN.",
  },
  {
    value: "ignore",
    label: "Ignore",
    help: "Purely organizational — no derived status, renders as a plain folder.",
  },
];

/**
 * MonitorGroupView JSON (internal/adapters/http/handlers/monitor_group.go),
 * snake_case, matches the existing handler style used by MonitorView etc.
 *
 * `status` is the derived status (domain.Status: 0=DOWN 1=UP 2=PENDING
 * 3=MAINTENANCE), null when the group has no derived status (ignore
 * condition, or no children). This is only trustworthy for the FIRST paint —
 * after that, recompute live from the WS heartbeat stream via
 * `resolveGroupStatuses` below (see task contract, "Frontend contract").
 */
export interface MonitorGroupView {
  id: number;
  name: string;
  description: string;
  /** Informational contact for the folder; monitors may inherit this. */
  owner?: string;
  parent_id: number | null;
  condition: GroupCondition;
  threshold: number;
  threshold_is_percent: boolean;
  weight: number;
  collapsed: boolean;
  /** Caller-specific: may a newly-created monitor be placed in this group? */
  can_create_monitor?: boolean;
  /** Full structural edit + delete (admin or creator). */
  can_edit?: boolean;
  /** Non-structural edit only (includes full editors). */
  can_edit_metadata?: boolean;
  status: number | null;
  created_at: string;
  updated_at: string;
}

export interface CreateMonitorGroupInput {
  name: string;
  description?: string;
  owner?: string;
  parent_id?: number | null;
  condition?: GroupCondition;
  threshold?: number;
  threshold_is_percent?: boolean;
  weight?: number;
  collapsed?: boolean;
}

export interface UpdateMonitorGroupInput extends Partial<CreateMonitorGroupInput> {}

export const monitorGroupsApi = {
  async list(): Promise<MonitorGroupView[]> {
    return api.get<MonitorGroupView[]>("/monitor-groups");
  },

  async get(id: number): Promise<MonitorGroupView> {
    return api.get<MonitorGroupView>(`/monitor-groups/${id}`);
  },

  async create(input: CreateMonitorGroupInput): Promise<MonitorGroupView> {
    return api.post<MonitorGroupView>("/monitor-groups", input);
  },

  async update(
    id: number,
    input: UpdateMonitorGroupInput,
  ): Promise<MonitorGroupView> {
    return api.put<MonitorGroupView>(`/monitor-groups/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/monitor-groups/${id}`);
  },

  /**
   * Notification providers attached to this group — i.e. the ones that fire on
   * the group's OWN derived status (its rollup condition), not the ones
   * attached to the monitors inside it.
   *
   * Mirrors GET /api/monitors/:id/notifications. Returns only what was linked
   * explicitly: a provider flagged `is_default` is never auto-attached to a
   * group, so it appears here only if someone ticked it. 404 when the group is
   * not visible to the caller.
   */
  async listNotifications(groupId: number): Promise<Notification[]> {
    return api.get<Notification[]>(`/monitor-groups/${groupId}/notifications`);
  },
};

// ─────────────────────────────────────────────────────────────────────────
// Rollup — faithful TS port of internal/core/domain/monitor_group.go.
//
// Keep this block in lockstep with the Go source. Any change there
// (maintenance-exclusion, the all-down floor, threshold cross-multiplication)
// must be mirrored here.
// ─────────────────────────────────────────────────────────────────────────

/** Matches domain.Status: 0=DOWN 1=UP 2=PENDING 3=MAINTENANCE. */
export type RollupStatus = 0 | 1 | 2 | 3;
export const ROLLUP_DOWN: RollupStatus = 0;
export const ROLLUP_UP: RollupStatus = 1;
export const ROLLUP_PENDING: RollupStatus = 2;
export const ROLLUP_MAINTENANCE: RollupStatus = 3;

interface StatusTally {
  down: number;
  up: number;
  /** Children NOT in maintenance — maintenance is excluded from every count. */
  active: number;
}

/** Port of monitor_group.go tallyStatuses. */
function tallyStatuses(children: RollupStatus[]): StatusTally {
  const t: StatusTally = { down: 0, up: 0, active: 0 };
  for (const s of children) {
    if (s === ROLLUP_MAINTENANCE) continue;
    t.active++;
    if (s === ROLLUP_DOWN) t.down++;
    else if (s === ROLLUP_UP) t.up++;
  }
  return t;
}

/**
 * Port of MonitorGroup.trips. Callers must have ruled out an all-maintenance
 * group (t.active === 0) first.
 */
function trips(
  group: Pick<
    MonitorGroupView,
    "condition" | "threshold" | "threshold_is_percent"
  >,
  t: StatusTally,
): boolean {
  // Floor: if every (non-maintenance) child is DOWN, the group is DOWN under
  // every condition. Without this a misconfigured threshold (5 required, only
  // 3 children) could never trip, and the group would claim UP with nothing
  // running.
  if (t.down === t.active) return true;

  switch (group.condition) {
    case "all_down":
      return false; // the all-down case is the floor above
    case "threshold": {
      // A threshold below 1 would trip on zero DOWN children. Treat it as 1 —
      // same as worst-of-children.
      const limit = Math.max(group.threshold, 1);
      if (group.threshold_is_percent) {
        // Cross-multiply rather than divide, to avoid float rounding drift.
        return t.down * 100 >= limit * t.active;
      }
      return t.down >= limit;
    }
    case "worst_of_children":
    default:
      return t.down > 0;
  }
}

/**
 * Port of MonitorGroup.Rollup. Derives a group's status from the statuses of
 * its DIRECT children — child monitors plus subgroups whose own status has
 * already been resolved (resolve bottom-up, deepest first — see
 * `resolveGroupStatuses`).
 *
 * `ok` is false when the group has no status to display: an "ignore" group,
 * or a group with no children at all. Callers render those as a plain
 * folder, no badge.
 */
export function rollupGroup(
  group: Pick<
    MonitorGroupView,
    "condition" | "threshold" | "threshold_is_percent"
  >,
  children: RollupStatus[],
): { status: RollupStatus; ok: boolean } {
  if (group.condition === "ignore" || children.length === 0) {
    return { status: ROLLUP_PENDING, ok: false };
  }

  const t = tallyStatuses(children);

  // Every child is in maintenance, so the group is too.
  if (t.active === 0) {
    return { status: ROLLUP_MAINTENANCE, ok: true };
  }

  if (trips(group, t)) {
    return { status: ROLLUP_DOWN, ok: true };
  }
  if (t.up > 0) {
    return { status: ROLLUP_UP, ok: true };
  }
  return { status: ROLLUP_PENDING, ok: true };
}

// ─────────────────────────────────────────────────────────────────────────
// Tree helpers shared by the monitors list page and the dashboard.
// ─────────────────────────────────────────────────────────────────────────

/** Sort groups the way the UI displays siblings: weight, then name. */
function sortGroups(groups: MonitorGroupView[]): MonitorGroupView[] {
  return [...groups].sort(
    (a, b) => a.weight - b.weight || a.name.localeCompare(b.name),
  );
}

/** Sort monitors the way the API lists them: weight, then name, then id. */
export function sortMonitors<
  M extends { id: number; name?: string; weight?: number },
>(monitors: M[]): M[] {
  return [...monitors].sort((a, b) => {
    const wa = a.weight ?? 2000;
    const wb = b.weight ?? 2000;
    if (wa !== wb) return wa - wb;
    const na = a.name ?? "";
    const nb = b.name ?? "";
    const byName = na.localeCompare(nb);
    if (byName !== 0) return byName;
    return a.id - b.id;
  });
}

/**
 * Indexes a flat group list (+ any monitor list carrying `id`/`group_id`)
 * into parent -> children maps, so callers can walk the tree without
 * re-deriving these buckets themselves. `null` is the top-level bucket key.
 */
export function indexGroupChildren<
  M extends {
    id: number;
    name?: string;
    weight?: number;
    group_id?: number | null;
  },
>(
  groups: MonitorGroupView[],
  monitors: M[],
): {
  subgroupsByParent: Map<number | null, MonitorGroupView[]>;
  monitorsByGroup: Map<number | null, M[]>;
} {
  const byParent = new Map<number | null, MonitorGroupView[]>();
  for (const g of groups) {
    const key = g.parent_id ?? null;
    const arr = byParent.get(key) ?? [];
    arr.push(g);
    byParent.set(key, arr);
  }
  for (const [key, arr] of byParent) byParent.set(key, sortGroups(arr));

  const monitorsByGroup = new Map<number | null, M[]>();
  for (const m of monitors) {
    const key = m.group_id ?? null;
    const arr = monitorsByGroup.get(key) ?? [];
    arr.push(m);
    monitorsByGroup.set(key, arr);
  }
  for (const [key, arr] of monitorsByGroup) {
    monitorsByGroup.set(key, sortMonitors(arr));
  }

  return { subgroupsByParent: byParent, monitorsByGroup };
}

/**
 * Resolves the derived status for every group in `groups`, bottom-up
 * (deepest first) so a group's own resolved status is available when its
 * ancestors are rolled up. Mirrors MonitorGroupService.ResolveStatuses:
 * groups with no status (ignore / empty, after excluding maintenance) are
 * absent from the returned map.
 *
 * `monitors` must carry each monitor's CURRENT rollup-input status — see
 * `monitorToRollupStatus` to derive one from live WS state.
 */
export function resolveGroupStatuses(
  groups: MonitorGroupView[],
  monitors: {
    id: number;
    group_id: number | null | undefined;
    status: RollupStatus;
  }[],
): Map<number, RollupStatus> {
  const groupById = new Map(groups.map((g) => [g.id, g]));
  const { subgroupsByParent, monitorsByGroup } = indexGroupChildren(
    groups,
    monitors,
  );

  const resolved = new Map<number, RollupStatus>();
  // Cycle guard: a group's own resolution should never depend on itself.
  // Cycles are rejected server-side, but a defensive guard here means a
  // stale/inconsistent client cache can't hang the tab.
  const visiting = new Set<number>();

  function resolve(id: number): RollupStatus | null {
    const cached = resolved.get(id);
    if (cached !== undefined) return cached;
    const group = groupById.get(id);
    if (!group || visiting.has(id)) return null;
    visiting.add(id);

    const childStatuses: RollupStatus[] = [];
    for (const m of monitorsByGroup.get(id) ?? []) childStatuses.push(m.status);
    for (const sub of subgroupsByParent.get(id) ?? []) {
      const s = resolve(sub.id);
      if (s !== null) childStatuses.push(s);
    }

    visiting.delete(id);
    const { status, ok } = rollupGroup(group, childStatuses);
    if (ok) resolved.set(id, status);
    return ok ? status : null;
  }

  for (const g of groups) resolve(g.id);
  return resolved;
}

/**
 * Derives a RollupStatus for a monitor from live WS state: prefer the latest
 * heartbeat status (the WS wire layer already normalizes a paused monitor's
 * heartbeat to "maintenance" — see normalizeWireStatus in ws.svelte.ts, which
 * is exactly the "excluded from tally" treatment a paused monitor should get
 * in a group rollup). Falls back to the monitor's own `status` field when no
 * heartbeat has been seen yet (e.g. a brand new monitor).
 */
export function monitorToRollupStatus(
  monitorStatus: "up" | "down" | "pending" | "maintenance" | "paused",
  heartbeatStatus?: "up" | "down" | "pending" | "maintenance",
): RollupStatus {
  const s =
    heartbeatStatus ??
    (monitorStatus === "paused" ? "maintenance" : monitorStatus);
  switch (s) {
    case "up":
      return ROLLUP_UP;
    case "down":
      return ROLLUP_DOWN;
    case "maintenance":
      return ROLLUP_MAINTENANCE;
    default:
      return ROLLUP_PENDING;
  }
}

/** Maps a RollupStatus (or null = no status) to the string union StatusPill expects. */
export function rollupStatusToPillStatus(
  status: RollupStatus | null | undefined,
): "up" | "down" | "pending" | "maintenance" {
  switch (status) {
    case ROLLUP_DOWN:
      return "down";
    case ROLLUP_UP:
      return "up";
    case ROLLUP_MAINTENANCE:
      return "maintenance";
    default:
      return "pending";
  }
}

/**
 * Flattens the group tree into an indented option list for pickers (group
 * form's "parent group", monitor form's "group"). When `excludeId` is given
 * (editing an existing group), that group and all of its descendants are
 * omitted — a group can't be nested under itself or its own child.
 */
export function buildGroupOptions(
  groups: MonitorGroupView[],
  excludeId?: number,
): { id: number; name: string; depth: number }[] {
  const excluded = new Set<number>();
  if (excludeId != null) {
    excluded.add(excludeId);
    const byParent = new Map<number, number[]>();
    for (const g of groups) {
      if (g.parent_id != null) {
        const arr = byParent.get(g.parent_id) ?? [];
        arr.push(g.id);
        byParent.set(g.parent_id, arr);
      }
    }
    const stack = [excludeId];
    while (stack.length > 0) {
      const id = stack.pop()!;
      for (const child of byParent.get(id) ?? []) {
        if (!excluded.has(child)) {
          excluded.add(child);
          stack.push(child);
        }
      }
    }
  }

  const visible = groups.filter((g) => !excluded.has(g.id));
  const byParent = new Map<number | null, MonitorGroupView[]>();
  for (const g of visible) {
    const key = g.parent_id ?? null;
    const arr = byParent.get(key) ?? [];
    arr.push(g);
    byParent.set(key, arr);
  }
  for (const [key, arr] of byParent) byParent.set(key, sortGroups(arr));

  const out: { id: number; name: string; depth: number }[] = [];
  function walk(parentId: number | null, depth: number) {
    for (const g of byParent.get(parentId) ?? []) {
      out.push({ id: g.id, name: g.name, depth });
      walk(g.id, depth + 1);
    }
  }
  walk(null, 0);
  return out;
}
