/**
 * Pure logic for the notification providers attached to a MONITOR GROUP.
 *
 * A group is an alerting entity in its own right: an attached notification
 * fires on the group's OWN derived status (the rollup its `condition`
 * produces), not once per monitor inside it. See the frozen contract —
 * `group_notifications` is a plain many-to-many join.
 *
 * The rule this module exists to enforce:
 *
 *   A notification flagged `is_default` ("auto-attach to new monitors") is
 *   NEVER auto-attached to a group. Not on group create, not on notification
 *   create. Every group checkbox starts UNCHECKED and must be ticked
 *   explicitly. `is_default` is display information here — never an input to
 *   the selection or to the diff.
 *
 * Framework-free and unit-tested in isolation (group-notifications.test.ts),
 * same pattern as monitor-filter.ts / monitor-detail-state.ts.
 */
import type { Notification } from "$lib/api/notifications";

/** The exact set of calls needed to move the server from `baseline` to `selected`. */
export interface NotificationLinkDiff {
  /** Notification IDs to POST   /api/notifications/:id/group/:groupId */
  attach: number[];
  /** Notification IDs to DELETE /api/notifications/:id/group/:groupId */
  detach: number[];
}

function sortedUnique(ids: Iterable<number>): number[] {
  return [...new Set(ids)].sort((a, b) => a - b);
}

/**
 * The set of checkboxes that start TICKED when the group form opens.
 *
 * - Creating a group (`attached` is null/undefined): NOTHING is ticked. A
 *   provider's `is_default` flag is deliberately ignored — that flag attaches
 *   it to new *monitors*, never to a group.
 * - Editing a group: exactly the providers the server says are attached, and
 *   nothing else. IDs the caller can no longer see (deleted provider, or one
 *   outside their scope) are dropped so the form can't try to re-attach or
 *   detach a phantom.
 */
export function initialGroupNotificationSelection(
  providers: readonly Pick<Notification, "id" | "is_default">[],
  attached: readonly number[] | null | undefined,
): number[] {
  if (!attached || attached.length === 0) return [];
  const known = new Set(providers.map((p) => p.id));
  return sortedUnique(attached).filter((id) => known.has(id));
}

/**
 * Diffs the ticked set against what is actually on the server, so a save
 * issues ONLY the calls it needs — never N detaches followed by N attaches.
 *
 * Both sides are deduped and sorted, so the result is deterministic regardless
 * of the order the user clicked the boxes.
 */
export function diffNotificationLinks(
  baseline: Iterable<number>,
  selected: Iterable<number>,
): NotificationLinkDiff {
  const before = new Set(baseline);
  const after = new Set(selected);
  return {
    attach: sortedUnique([...after].filter((id) => !before.has(id))),
    detach: sortedUnique([...before].filter((id) => !after.has(id))),
  };
}

/** True when the diff would issue at least one call. */
export function hasLinkChanges(diff: NotificationLinkDiff): boolean {
  return diff.attach.length > 0 || diff.detach.length > 0;
}

/** The two API calls `syncGroupNotifications` drives. Injected so it stays testable. */
export interface NotificationLinkOps {
  attach(notificationId: number, groupId: number): Promise<void>;
  detach(notificationId: number, groupId: number): Promise<void>;
}

export interface NotificationLinkFailure {
  id: number;
  op: "attach" | "detach";
  message: string;
}

/** What ACTUALLY landed on the server. Never a claim about what was attempted. */
export interface NotificationLinkSyncResult {
  attached: number[];
  detached: number[];
  failures: NotificationLinkFailure[];
}

/**
 * Applies a diff, one call at a time, and reports each outcome individually.
 *
 * Deliberately does NOT throw and does NOT abort on the first failure: a
 * half-applied save is a real state the user has to be told about accurately.
 * The caller reports the failures and re-baselines from `attached`/`detached`
 * (see `applySyncToBaseline`) so a retry re-diffs against reality instead of
 * replaying calls that already succeeded.
 */
export async function syncGroupNotifications(
  groupId: number,
  diff: NotificationLinkDiff,
  ops: NotificationLinkOps,
): Promise<NotificationLinkSyncResult> {
  const result: NotificationLinkSyncResult = {
    attached: [],
    detached: [],
    failures: [],
  };

  for (const id of diff.detach) {
    try {
      await ops.detach(id, groupId);
      result.detached.push(id);
    } catch (err: unknown) {
      result.failures.push({ id, op: "detach", message: errorMessage(err) });
    }
  }

  for (const id of diff.attach) {
    try {
      await ops.attach(id, groupId);
      result.attached.push(id);
    } catch (err: unknown) {
      result.failures.push({ id, op: "attach", message: errorMessage(err) });
    }
  }

  return result;
}

/**
 * The new server-truth baseline after a sync: what was there, plus what really
 * attached, minus what really detached. Failed calls change nothing.
 */
export function applySyncToBaseline(
  baseline: Iterable<number>,
  result: NotificationLinkSyncResult,
): number[] {
  const next = new Set(baseline);
  for (const id of result.detached) next.delete(id);
  for (const id of result.attached) next.add(id);
  return sortedUnique(next);
}

/** ApiError from $lib/api/client is a plain object with `message` — not an Error. */
function errorMessage(err: unknown): string {
  if (err && typeof err === "object" && "message" in err) {
    const msg = (err as { message?: unknown }).message;
    if (typeof msg === "string" && msg) return msg;
  }
  return "Request failed";
}
