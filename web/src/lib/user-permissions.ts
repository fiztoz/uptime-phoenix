/**
 * Pure logic behind the admin grant editor (Settings → Users → "Access").
 *
 * The wire contract is `PUT /api/users/:id/permissions` with
 * `{ monitor_ids, groups }`, and it is a REPLACE-SET: whatever you send
 * becomes the user's entire grant. There is no add/remove endpoint. So the
 * editor holds a draft set locally and ships the whole thing on save — which
 * makes "is this draft different from what the server has?" a real question,
 * hence `permissionsEqual`.
 *
 * The other thing worth stating plainly, because the UI has to explain it:
 * **a group grant reaches as far as its `include_descendants` flag says** —
 * mirroring `AccessService.resolveScope` on the server
 * (internal/core/services/access_service.go):
 *
 *   deep    — the folder, every subfolder, and every monitor in any of them.
 *   shallow — the folder and the monitors filed directly in it. Nothing below.
 *
 * `coveredMonitorIds` is the client-side echo of that rule, used only to tell
 * the admin what they are about to hand over. It is a preview, never an
 * authorization decision: the server is the choke point.
 *
 * Two things this module does NOT model, deliberately:
 *
 *   - ADMINS. An admin is unrestricted regardless of what is stored here (and
 *     their raw capability flags read `false` while they may still do
 *     everything). Callers must gate on `is_admin || can_x` — see
 *     docs/HANDOFF-NEXT.md §3.1.
 *   - WRITE access. Everything here is about what a user can SEE. Whether they
 *     may change a monitor is ownership (`monitor.user_id`), which is not a
 *     grant and is not editable from this screen.
 */
import type { MonitorGroupView } from "$lib/api/monitorGroups";
import type { GroupGrant, UserPermissions } from "$lib/api/users";

/** The slice of a monitor this module needs. `Monitor` from the WS store does
 * not declare `group_id`, but the wire always sends it — see MonitorWithGroup. */
export interface GrantableMonitor {
  id: number;
  name: string;
  group_id?: number | null;
}

/** The slice of a group this module needs. */
export type GrantableGroup = Pick<
  MonitorGroupView,
  "id" | "name" | "parent_id"
>;

/**
 * Renders a group as its full ancestry path — "Platform / EU / Edge" — so two
 * subgroups that are both called "Edge" are distinguishable in a flat list.
 *
 * Walks up `parent_id`, guarding against a cycle (rejected server-side, but a
 * stale client cache must not hang the tab) and against a dangling parent.
 */
export function groupPath(
  groups: GrantableGroup[],
  group: GrantableGroup,
): string {
  const byId = new Map(groups.map((g) => [g.id, g]));
  const names = [group.name];
  const seen = new Set<number>([group.id]);
  let parentId = group.parent_id;
  while (parentId != null && !seen.has(parentId)) {
    const parent = byId.get(parentId);
    if (!parent) break;
    seen.add(parent.id);
    names.unshift(parent.name);
    parentId = parent.parent_id;
  }
  return names.join(" / ");
}

/**
 * Expands group grants into every group they reach: deep grants pull in their
 * whole subtree, shallow grants contribute only themselves.
 *
 * Mirrors `AccessService.resolveScope`, including the union rule — grants are
 * additive and there is no deny, so a shallow grant on a folder already inside a
 * deep grant's subtree takes nothing back. If you want to know why, the server's
 * copy of this comment is the authority.
 *
 * A granted group ID that no longer exists is dropped (the group was deleted out
 * from under a stale draft) — it contributes nothing and expands to nothing.
 */
export function expandGroupIds(
  groups: GrantableGroup[],
  grants: Iterable<GroupGrant>,
): Set<number> {
  const childrenOf = new Map<number, number[]>();
  const existing = new Set<number>();
  for (const g of groups) {
    existing.add(g.id);
    if (g.parent_id != null) {
      const arr = childrenOf.get(g.parent_id) ?? [];
      arr.push(g.id);
      childrenOf.set(g.parent_id, arr);
    }
  }

  const reached = new Set<number>();
  const stack: number[] = [];
  const shallow: number[] = [];
  for (const grant of grants) {
    if (!existing.has(grant.group_id)) continue;
    if (grant.include_descendants) {
      stack.push(grant.group_id); // walked below
    } else {
      shallow.push(grant.group_id); // merged AFTER the walk — see below
    }
  }

  // Walk the deep grants FIRST, with only deep-reached groups in `reached`.
  //
  // The shallow ids must not be seeded into `reached` before this loop, however
  // natural that looks. `reached` doubles as the visited set, so a folder sitting
  // in it when the walk arrives is treated as already expanded and its children
  // are never queued. Pre-seeding a shallow grant on a folder that a deep grant
  // also covers would therefore truncate the deep grant at that folder — the
  // shallow grant would silently narrow it, which is precisely what grants cannot
  // do. Mirrors resolveScope in access_service.go, which unions shallow after
  // expand() for the same reason.
  while (stack.length > 0) {
    const id = stack.pop()!;
    if (reached.has(id)) continue; // also the cycle guard
    reached.add(id);
    for (const child of childrenOf.get(id) ?? []) stack.push(child);
  }
  for (const id of shallow) reached.add(id);
  return reached;
}

/**
 * Every monitor ID the granted GROUPS cover — i.e. what the user gains without
 * anyone naming those monitors individually. Direct monitor grants are NOT
 * included; `grantSummary` combines the two.
 *
 * Note this covers monitors filed directly in a SHALLOW-granted folder too: the
 * folder is in the reached set, so its own monitors count. Only its subfolders'
 * monitors are excluded. That matches the server.
 */
export function coveredMonitorIds(
  groups: GrantableGroup[],
  monitors: GrantableMonitor[],
  grants: Iterable<GroupGrant>,
): Set<number> {
  const reached = expandGroupIds(groups, grants);
  const covered = new Set<number>();
  if (reached.size === 0) return covered;
  for (const m of monitors) {
    if (m.group_id != null && reached.has(m.group_id)) covered.add(m.id);
  }
  return covered;
}

export interface GrantSummary {
  /** Monitors named individually in the grant. */
  direct: number;
  /** Monitors reached only through a granted group (never double-counted). */
  viaGroups: number;
  /** Distinct monitors the user can see in total. */
  total: number;
  /** Groups explicitly granted (not counting subgroups pulled in with them). */
  groups: number;
  /**
   * Direct monitor grants that a granted group already covers. Harmless — the
   * server unions them — but worth telling the admin, because removing the
   * group would NOT remove access to these.
   */
  redundantDirect: number;
}

/**
 * What this grant actually adds up to, for the "sees N monitors" line under
 * the editor. A monitor granted both directly and via a group is one monitor.
 */
export function grantSummary(
  groups: GrantableGroup[],
  monitors: GrantableMonitor[],
  permissions: UserPermissions,
): GrantSummary {
  const known = new Set(monitors.map((m) => m.id));
  const direct = new Set(
    [...permissions.monitor_ids].filter((id) => known.has(id)),
  );
  const covered = coveredMonitorIds(groups, monitors, permissions.groups);

  let redundantDirect = 0;
  for (const id of direct) {
    if (covered.has(id)) redundantDirect++;
  }

  const union = new Set([...direct, ...covered]);
  return {
    direct: direct.size,
    viaGroups: union.size - direct.size,
    total: union.size,
    groups: new Set(permissions.groups.map((g) => g.group_id)).size,
    redundantDirect,
  };
}

/**
 * Deduped and sorted, so two equivalent sets serialize identically.
 *
 * Group grants dedupe by id, FIRST occurrence wins — matching the server's
 * dedupeGroupGrants. Two entries for one folder are contradictory rather than
 * mergeable, and the two sides must resolve them the same way or the dirty check
 * disagrees with what actually got stored.
 */
export function normalizePermissions(
  permissions: UserPermissions,
): UserPermissions {
  const seen = new Set<number>();
  const groups: GroupGrant[] = [];
  for (const g of permissions.groups) {
    if (seen.has(g.group_id)) continue;
    seen.add(g.group_id);
    groups.push({
      group_id: g.group_id,
      include_descendants: g.include_descendants,
    });
  }
  groups.sort((a, b) => a.group_id - b.group_id);
  return {
    monitor_ids: [...new Set(permissions.monitor_ids)].sort((a, b) => a - b),
    groups,
  };
}

/**
 * Set equality, order- and duplicate-insensitive — the editor's dirty check.
 * The draft is compared against the last set the server acknowledged, so the
 * Save button can be disabled when a click would be a no-op REPLACE.
 *
 * `include_descendants` counts. Flipping a folder from deep to shallow changes
 * nothing about WHICH folders are granted, so an id-only comparison would call
 * the draft clean, grey out Save, and silently strand the admin's change.
 */
export function permissionsEqual(
  a: UserPermissions,
  b: UserPermissions,
): boolean {
  const na = normalizePermissions(a);
  const nb = normalizePermissions(b);
  return (
    na.monitor_ids.length === nb.monitor_ids.length &&
    na.groups.length === nb.groups.length &&
    na.monitor_ids.every((id, i) => id === nb.monitor_ids[i]) &&
    na.groups.every(
      (g, i) =>
        g.group_id === nb.groups[i].group_id &&
        g.include_descendants === nb.groups[i].include_descendants,
    )
  );
}

/**
 * The groups a picker should offer, each with its display path, sorted by that
 * path so a parent sorts immediately above its children.
 */
export function groupOptions(
  groups: GrantableGroup[],
): { id: number; path: string }[] {
  return groups
    .map((g) => ({ id: g.id, path: groupPath(groups, g) }))
    .sort((a, b) => a.path.localeCompare(b.path));
}
