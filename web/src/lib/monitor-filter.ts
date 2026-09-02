/**
 * Pure helpers for dashboard filtering — search / group / tag / status / type,
 * plus the group-subtree expansion and the recursive group rollup COUNTS that
 * the group cards display. Framework-free and unit-tested in isolation
 * (monitor-filter.test.ts), same pattern as monitor-detail-state.ts.
 *
 * Deliberately NOT here: a group's derived STATUS. That is the backend's
 * rollup semantics (maintenance-exclusion / threshold / all-down) and already
 * has a faithful port in $lib/api/monitorGroups.ts — use `resolveGroupStatuses`
 * for the status pill and this module for the counts.
 */
import type { MonitorGroupView } from "$lib/api/monitorGroups";

/** Monitor status union, mirroring Monitor["status"] in $lib/stores/ws.svelte.ts. */
export type MonitorStatus =
  | "up"
  | "down"
  | "pending"
  | "maintenance"
  | "paused";

export const STATUS_FILTERS: readonly MonitorStatus[] = [
  "up",
  "down",
  "pending",
  "maintenance",
  "paused",
];

export type DashboardSort =
  | "default"
  | "status"
  | "name-asc"
  | "name-desc"
  | "response-asc"
  | "response-desc";

export const DASHBOARD_SORTS: readonly DashboardSort[] = [
  "default",
  "status",
  "name-asc",
  "name-desc",
  "response-asc",
  "response-desc",
];

/** Default urgency order when status sorting is first selected. */
export const DEFAULT_STATUS_ORDER: readonly MonitorStatus[] = [
  "down",
  "pending",
  "maintenance",
  "paused",
  "up",
];

/** Keep valid statuses once each, then append any omitted statuses. */
export function normalizeStatusOrder(
  order: readonly string[],
): MonitorStatus[] {
  const valid = new Set<string>(STATUS_FILTERS);
  const seen = new Set<MonitorStatus>();
  const normalized: MonitorStatus[] = [];
  for (const raw of order) {
    if (!valid.has(raw)) continue;
    const status = raw as MonitorStatus;
    if (seen.has(status)) continue;
    seen.add(status);
    normalized.push(status);
  }
  for (const status of DEFAULT_STATUS_ORDER) {
    if (!seen.has(status)) normalized.push(status);
  }
  return normalized;
}

/**
 * The minimum a monitor must expose to be filtered. Structural, so both the
 * WS `Monitor` and the richer `MonitorWithTags` ($lib/api/monitors) satisfy it.
 *
 * `tags` is typed `unknown` ON PURPOSE: the field is mid-migration on the
 * backend (it does not ship it at all today) and older code models it as bare
 * name strings. `monitorTags` below normalizes whatever actually arrives.
 */
export interface FilterableMonitor {
  id: number;
  name: string;
  type: string;
  status: MonitorStatus;
  target?: string;
  group_id?: number | null;
  tags?: unknown;
}

/** A tag as embedded on a monitor payload, after normalization. */
export interface NormalizedTag {
  id: number;
  name: string;
  color: string;
  value: string;
}

/** Sentinel group filter meaning "monitors that belong to no group at all". */
export const UNGROUPED = "none";

/** null = no group filter; a number = that group (and its descendants). */
export type GroupFilter = number | typeof UNGROUPED | null;

export interface FilterCriteria {
  /** Case-insensitive "contains", matched against monitor name AND target/URL. */
  search: string;
  group: GroupFilter;
  /**
   * Tag NAMES (not ids) — stable across the backend's id churn and URL-readable.
   * Multi-select: a monitor matches if it carries ANY of the selected tags (OR).
   */
  tags: string[];
  /**
   * Selected statuses. Multi-select: a monitor matches if its status is ANY of
   * these (OR). Empty array = no status filter (all statuses).
   */
  statuses: MonitorStatus[];
  type: string;
  /** Sorting is view state, not a filter, so it does not affect active-filter counts. */
  sort: DashboardSort;
  /** Complete, user-configurable priority used when `sort === "status"`. */
  statusOrder: MonitorStatus[];
}

export const EMPTY_CRITERIA: FilterCriteria = {
  search: "",
  group: null,
  tags: [],
  statuses: [],
  type: "",
  sort: "default",
  statusOrder: [...DEFAULT_STATUS_ORDER],
};

export function hasActiveFilters(c: FilterCriteria): boolean {
  return (
    c.search.trim() !== "" ||
    c.group !== null ||
    c.tags.length > 0 ||
    c.statuses.length > 0 ||
    c.type !== ""
  );
}

/**
 * True when a numeric group selection is acting as folder navigation rather
 * than as one facet in a global monitor search. In this mode the dashboard
 * keeps immediate subgroup cards visible and shows only directly assigned
 * monitors; any explicit search, facet, or sort switches back to flat results.
 */
export function isGroupBrowseCriteria(c: FilterCriteria): boolean {
  return (
    typeof c.group === "number" &&
    c.search.trim() === "" &&
    c.tags.length === 0 &&
    c.statuses.length === 0 &&
    c.type === "" &&
    c.sort === "default"
  );
}

// ─────────────────────────────────────────────────────────────────────────
// Tags — read defensively.
// ─────────────────────────────────────────────────────────────────────────

/**
 * Normalizes a monitor's `tags` into a consistent shape, tolerating every
 * wire shape we might be handed while the backend lands the field:
 *
 *   - `["prod", "edge"]`                         legacy: bare tag names
 *   - `[{ id, name, color, value }]`             the embedded shape we expect
 *   - `[{ tag_id, value, tag: { id, name, … } }] ` the join-row shape used by
 *                                                 GET /monitors/:id/tags
 *
 * Anything unrecognized is dropped rather than thrown on — a monitor with a
 * malformed tag must still render.
 */
export function monitorTags(monitor: { tags?: unknown }): NormalizedTag[] {
  const raw = monitor.tags;
  if (!Array.isArray(raw)) return [];

  const out: NormalizedTag[] = [];
  for (const entry of raw) {
    if (typeof entry === "string") {
      if (entry) out.push({ id: 0, name: entry, color: "", value: "" });
      continue;
    }
    if (!entry || typeof entry !== "object") continue;

    const o = entry as Record<string, unknown>;
    const nested =
      o.tag && typeof o.tag === "object"
        ? (o.tag as Record<string, unknown>)
        : null;

    const nameRaw = o.name ?? nested?.name;
    if (typeof nameRaw !== "string" || nameRaw === "") continue;

    const idRaw = Number(o.id ?? o.tag_id ?? nested?.id ?? 0);
    const colorRaw = o.color ?? nested?.color;
    const valueRaw = o.value;

    out.push({
      id: Number.isFinite(idRaw) ? idRaw : 0,
      name: nameRaw,
      color: typeof colorRaw === "string" ? colorRaw : "",
      value: typeof valueRaw === "string" ? valueRaw : "",
    });
  }
  return out;
}

/** Distinct tags across every monitor, sorted by name — powers the tag filter. */
export function collectTags(monitors: FilterableMonitor[]): NormalizedTag[] {
  const byName = new Map<string, NormalizedTag>();
  for (const m of monitors) {
    for (const t of monitorTags(m)) {
      // First writer wins, but let a later entry supply a color the first lacked.
      const seen = byName.get(t.name);
      if (!seen) byName.set(t.name, t);
      else if (!seen.color && t.color)
        byName.set(t.name, { ...seen, color: t.color });
    }
  }
  return [...byName.values()].sort((a, b) => a.name.localeCompare(b.name));
}

/** Distinct monitor types actually present, sorted — powers the type filter. */
export function collectTypes(monitors: FilterableMonitor[]): string[] {
  const types = new Set<string>();
  for (const m of monitors) if (m.type) types.add(m.type);
  return [...types].sort((a, b) => a.localeCompare(b));
}

// ─────────────────────────────────────────────────────────────────────────
// Group subtree expansion.
// ─────────────────────────────────────────────────────────────────────────

/**
 * All group ids in the subtree rooted at `rootId`, INCLUDING `rootId` itself.
 * Filtering by a group must match its descendant subgroups too, otherwise
 * drilling into a parent shows nothing.
 *
 * Cycle-safe: a corrupt/stale client cache with a parent loop can't hang us.
 */
export function groupSubtreeIds(
  groups: MonitorGroupView[],
  rootId: number,
): Set<number> {
  const byParent = new Map<number, number[]>();
  for (const g of groups) {
    if (g.parent_id == null) continue;
    const arr = byParent.get(g.parent_id) ?? [];
    arr.push(g.id);
    byParent.set(g.parent_id, arr);
  }

  const ids = new Set<number>([rootId]);
  const stack = [rootId];
  while (stack.length > 0) {
    const id = stack.pop()!;
    for (const child of byParent.get(id) ?? []) {
      if (ids.has(child)) continue; // also the cycle guard
      ids.add(child);
      stack.push(child);
    }
  }
  return ids;
}

/** Every monitor inside a group, recursing into its subgroups. */
export function monitorsInGroup<M extends FilterableMonitor>(
  monitors: M[],
  groups: MonitorGroupView[],
  groupId: number,
): M[] {
  const ids = groupSubtreeIds(groups, groupId);
  return monitors.filter((m) => m.group_id != null && ids.has(m.group_id));
}

/** Path from the root down to `groupId` — for the drill-in breadcrumb. */
export function groupPath(
  groups: MonitorGroupView[],
  groupId: number,
): MonitorGroupView[] {
  const byId = new Map(groups.map((g) => [g.id, g]));
  const path: MonitorGroupView[] = [];
  const seen = new Set<number>();

  let current = byId.get(groupId);
  while (current && !seen.has(current.id)) {
    seen.add(current.id);
    path.unshift(current);
    current =
      current.parent_id == null ? undefined : byId.get(current.parent_id);
  }
  return path;
}

// ─────────────────────────────────────────────────────────────────────────
// The filter itself.
// ─────────────────────────────────────────────────────────────────────────

export function filterMonitors<M extends FilterableMonitor>(
  monitors: M[],
  criteria: FilterCriteria,
  groups: MonitorGroupView[] = [],
): M[] {
  let list = monitors;

  const q = criteria.search.trim().toLowerCase();
  if (q) {
    list = list.filter(
      (m) =>
        m.name.toLowerCase().includes(q) ||
        (m.target ?? "").toLowerCase().includes(q),
    );
  }

  if (criteria.group === UNGROUPED) {
    list = list.filter((m) => m.group_id == null);
  } else if (typeof criteria.group === "number") {
    const ids = groupSubtreeIds(groups, criteria.group);
    list = list.filter((m) => m.group_id != null && ids.has(m.group_id));
  }

  if (criteria.tags.length > 0) {
    const selected = criteria.tags.map((t) => t.toLowerCase());
    list = list.filter((m) =>
      monitorTags(m).some((t) => selected.includes(t.name.toLowerCase())),
    );
  }

  if (criteria.statuses.length > 0) {
    const selected = new Set(criteria.statuses);
    list = list.filter((m) => selected.has(m.status));
  }

  if (criteria.type) {
    list = list.filter((m) => m.type === criteria.type);
  }

  return list;
}

/**
 * Sort an already deterministically ordered monitor list. The incoming order
 * is the tie-break, preserving manual weight/name/id ordering from sortMonitors.
 */
export function sortDashboardMonitors<M extends FilterableMonitor>(
  monitors: M[],
  sort: DashboardSort,
  statusOrder: readonly MonitorStatus[],
  responseTime: (monitor: M) => number | null | undefined = () => null,
): M[] {
  const list = [...monitors];
  if (sort === "default") return list;

  const originalIndex = new Map(
    list.map((monitor, index) => [monitor.id, index]),
  );
  const tieBreak = (a: M, b: M) =>
    (originalIndex.get(a.id) ?? 0) - (originalIndex.get(b.id) ?? 0);

  if (sort === "status") {
    const priority = new Map(
      normalizeStatusOrder(statusOrder).map((status, index) => [status, index]),
    );
    return list.sort(
      (a, b) =>
        (priority.get(a.status) ?? Number.MAX_SAFE_INTEGER) -
          (priority.get(b.status) ?? Number.MAX_SAFE_INTEGER) || tieBreak(a, b),
    );
  }

  if (sort === "name-asc" || sort === "name-desc") {
    const direction = sort === "name-asc" ? 1 : -1;
    return list.sort(
      (a, b) => direction * a.name.localeCompare(b.name) || tieBreak(a, b),
    );
  }

  const direction = sort === "response-asc" ? 1 : -1;
  return list.sort((a, b) => {
    const rawA = responseTime(a);
    const rawB = responseTime(b);
    const pingA =
      rawA != null && Number.isFinite(rawA) && rawA > 0 ? rawA : null;
    const pingB =
      rawB != null && Number.isFinite(rawB) && rawB > 0 ? rawB : null;
    if (pingA === null && pingB === null) return tieBreak(a, b);
    if (pingA === null) return 1;
    if (pingB === null) return -1;
    return direction * (pingA - pingB) || tieBreak(a, b);
  });
}

// ─────────────────────────────────────────────────────────────────────────
// Counts for the group cards.
// ─────────────────────────────────────────────────────────────────────────

export interface MonitorTally {
  total: number;
  up: number;
  down: number;
  pending: number;
  /** Paused + in-maintenance: present, but deliberately not being checked. */
  idle: number;
}

export interface GroupSummary extends MonitorTally {
  /** Direct subgroups only — what the card shows as "N subgroups". */
  subgroups: number;
}

export function tallyMonitors(monitors: FilterableMonitor[]): MonitorTally {
  const t: MonitorTally = { total: 0, up: 0, down: 0, pending: 0, idle: 0 };
  for (const m of monitors) {
    t.total++;
    switch (m.status) {
      case "up":
        t.up++;
        break;
      case "down":
        t.down++;
        break;
      case "pending":
        t.pending++;
        break;
      default:
        t.idle++; // maintenance | paused
    }
  }
  return t;
}

/**
 * Rolls up the counts a group card shows. Monitors are counted RECURSIVELY
 * (every subgroup's monitors included) so the card summarizes "everything
 * inside", while `subgroups` counts only the direct children.
 */
export function summarizeGroup(
  groups: MonitorGroupView[],
  monitors: FilterableMonitor[],
  groupId: number,
): GroupSummary {
  const tally = tallyMonitors(monitorsInGroup(monitors, groups, groupId));
  const subgroups = groups.filter((g) => g.parent_id === groupId).length;
  return { ...tally, subgroups };
}

// ─────────────────────────────────────────────────────────────────────────
// URL codec — filters live in the query string so a filtered view survives a
// refresh and can be shared/linked.
//
//   ?q=<text>&group=<id|none>&statuses=<s1,s2>&tags=<t1,t2>&type=<type>
//
// Backward-compat: bare ?status=X and ?tag=X (single) are parsed as a
// one-element list and silently re-encoded to the new form on next navigation.
//
// Absent key = that filter is off. Unrelated query params are preserved.
// ─────────────────────────────────────────────────────────────────────────

const FILTER_KEYS = [
  "q",
  "group",
  "statuses",
  "tags",
  "type",
  "sort",
  "status_order",
] as const;

const VALID_STATUSES = new Set<string>(STATUS_FILTERS);
const VALID_SORTS = new Set<string>(DASHBOARD_SORTS);

export function criteriaFromParams(params: URLSearchParams): FilterCriteria {
  const groupRaw = params.get("group");
  let group: GroupFilter = null;
  if (groupRaw === UNGROUPED) {
    group = UNGROUPED;
  } else if (groupRaw) {
    const n = Number(groupRaw);
    if (Number.isInteger(n) && n > 0) group = n;
  }

  // Multi-select: ?statuses=up,down  (new format)
  // Backward-compat: ?status=up      (old single-value format, parsed as [up])
  const statusesParam = params.get("statuses");
  const legacyStatus = params.get("status");
  let statuses: MonitorStatus[] = [];
  if (statusesParam) {
    statuses = statusesParam
      .split(",")
      .filter((s) => VALID_STATUSES.has(s))
      .map((s) => s as MonitorStatus);
  } else if (legacyStatus && VALID_STATUSES.has(legacyStatus)) {
    statuses = [legacyStatus as MonitorStatus];
  }

  const tagsParam = params.get("tags");
  const legacyTag = params.get("tag");
  let tags: string[] = [];
  if (tagsParam) {
    tags = tagsParam.split(",").filter((t) => t.length > 0);
  } else if (legacyTag) {
    tags = [legacyTag];
  }

  const sortParam = params.get("sort");
  const sort = VALID_SORTS.has(sortParam ?? "")
    ? (sortParam as DashboardSort)
    : "default";
  const statusOrderParam = params.get("status_order");
  const statusOrder = normalizeStatusOrder(
    statusOrderParam ? statusOrderParam.split(",") : DEFAULT_STATUS_ORDER,
  );

  return {
    search: params.get("q") ?? "",
    group,
    tags,
    statuses,
    type: params.get("type") ?? "",
    sort,
    statusOrder,
  };
}

/** Writes `criteria` into a copy of `params`, leaving unrelated keys alone. */
export function applyCriteriaToParams(
  params: URLSearchParams,
  criteria: FilterCriteria,
): URLSearchParams {
  const next = new URLSearchParams(params);
  for (const key of FILTER_KEYS) next.delete(key);

  const q = criteria.search.trim();
  if (q) next.set("q", q);
  if (criteria.group === UNGROUPED) next.set("group", UNGROUPED);
  else if (typeof criteria.group === "number")
    next.set("group", String(criteria.group));
  if (criteria.tags.length > 0) next.set("tags", criteria.tags.join(","));
  if (criteria.statuses.length > 0)
    next.set("statuses", criteria.statuses.join(","));
  if (criteria.type) next.set("type", criteria.type);
  if (criteria.sort !== "default") next.set("sort", criteria.sort);
  const statusOrder = normalizeStatusOrder(criteria.statusOrder);
  const hasCustomStatusOrder = statusOrder.some(
    (status, index) => status !== DEFAULT_STATUS_ORDER[index],
  );
  if (criteria.sort === "status" || hasCustomStatusOrder)
    next.set("status_order", statusOrder.join(","));

  return next;
}

/** The `?…` string for a criteria set — empty string when no filter is active. */
export function criteriaToSearchString(
  params: URLSearchParams,
  criteria: FilterCriteria,
): string {
  const qs = applyCriteriaToParams(params, criteria).toString();
  return qs ? `?${qs}` : "";
}
