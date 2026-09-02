/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  applySyncToBaseline,
  diffNotificationLinks,
  hasLinkChanges,
  initialGroupNotificationSelection,
  syncGroupNotifications,
  type NotificationLinkOps,
} from "./group-notifications";
import type { Notification } from "$lib/api/notifications";

function provider(id: number, over: Partial<Notification> = {}): Notification {
  return {
    id,
    name: `provider-${id}`,
    type: "telegram",
    config: {},
    active: true,
    is_default: false,
    include_ack_url: true,
    template_id: null,
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...over,
  };
}

/** A default-flagged provider — the one that must never auto-attach to a group. */
const DEFAULT_PROVIDER = provider(7, {
  name: "On-call PagerDuty",
  is_default: true,
});

/** Records every call so a test can assert on the exact calls made — or on none. */
function recordingOps(
  fail: { attach?: Set<number>; detach?: Set<number> } = {},
) {
  const calls: { op: "attach" | "detach"; id: number; groupId: number }[] = [];
  const ops: NotificationLinkOps = {
    async attach(notificationId, groupId) {
      calls.push({ op: "attach", id: notificationId, groupId });
      if (fail.attach?.has(notificationId))
        throw { message: `attach ${notificationId} boom` };
    },
    async detach(notificationId, groupId) {
      calls.push({ op: "detach", id: notificationId, groupId });
      if (fail.detach?.has(notificationId))
        throw { message: `detach ${notificationId} boom` };
    },
  };
  return { calls, ops };
}

describe("initialGroupNotificationSelection", () => {
  test("creating a group ticks NOTHING — an is_default provider is not pre-attached", () => {
    const providers = [
      provider(1),
      DEFAULT_PROVIDER,
      provider(9, { is_default: true }),
    ];
    expect(initialGroupNotificationSelection(providers, null)).toEqual([]);
    expect(initialGroupNotificationSelection(providers, undefined)).toEqual([]);
    expect(initialGroupNotificationSelection(providers, [])).toEqual([]);
  });

  test("editing ticks exactly the attached set, and nothing else", () => {
    const providers = [provider(1), provider(2), DEFAULT_PROVIDER];
    expect(initialGroupNotificationSelection(providers, [2])).toEqual([2]);
  });

  test("an is_default provider is ticked on edit only because it was explicitly attached", () => {
    const providers = [provider(1), DEFAULT_PROVIDER];
    expect(initialGroupNotificationSelection(providers, [7])).toEqual([7]);
    // ...and stays unticked when it was not.
    expect(initialGroupNotificationSelection(providers, [1])).toEqual([1]);
  });

  test("drops attached IDs the caller can no longer see (deleted or out of scope)", () => {
    const providers = [provider(1), provider(2)];
    expect(initialGroupNotificationSelection(providers, [2, 404])).toEqual([2]);
  });

  test("dedupes and sorts", () => {
    const providers = [provider(1), provider(2), provider(3)];
    expect(initialGroupNotificationSelection(providers, [3, 1, 3])).toEqual([
      1, 3,
    ]);
  });
});

describe("diffNotificationLinks", () => {
  test("no-op when the ticked set is unchanged", () => {
    const diff = diffNotificationLinks([1, 2, 3], [3, 2, 1]);
    expect(diff).toEqual({ attach: [], detach: [] });
    expect(hasLinkChanges(diff)).toBe(false);
  });

  test("no-op on an empty form (create, nothing ticked)", () => {
    const diff = diffNotificationLinks([], []);
    expect(diff).toEqual({ attach: [], detach: [] });
    expect(hasLinkChanges(diff)).toBe(false);
  });

  test("attach only", () => {
    const diff = diffNotificationLinks([1], [1, 4, 2]);
    expect(diff).toEqual({ attach: [2, 4], detach: [] });
    expect(hasLinkChanges(diff)).toBe(true);
  });

  test("detach only", () => {
    const diff = diffNotificationLinks([1, 2, 3], [2]);
    expect(diff).toEqual({ attach: [], detach: [1, 3] });
    expect(hasLinkChanges(diff)).toBe(true);
  });

  test("attach and detach in the same save", () => {
    const diff = diffNotificationLinks([1, 2], [2, 3]);
    expect(diff).toEqual({ attach: [3], detach: [1] });
  });

  test("an unchanged provider is never detached-then-reattached", () => {
    // The whole point of diffing: id 2 is on both sides and must appear in neither list.
    const diff = diffNotificationLinks([1, 2], [2, 3]);
    expect(diff.detach).not.toContain(2);
    expect(diff.attach).not.toContain(2);
  });

  test("dedupes duplicate ticks", () => {
    expect(diffNotificationLinks([], [5, 5, 5])).toEqual({
      attach: [5],
      detach: [],
    });
  });
});

describe("is_default never attaches to a group (the bug this prevents)", () => {
  test("create: a default provider left unticked produces ZERO attach calls", async () => {
    const providers = [provider(3), DEFAULT_PROVIDER];

    // Create path: no group exists yet, so the baseline is empty.
    const baseline = initialGroupNotificationSelection(providers, null);
    expect(baseline).toEqual([]);

    // The user ticks only provider 3. The default (7) is left alone.
    const selected = [3];
    const diff = diffNotificationLinks(baseline, selected);
    expect(diff.attach).toEqual([3]);
    expect(diff.attach).not.toContain(DEFAULT_PROVIDER.id);
    expect(diff.detach).toEqual([]);

    const { calls, ops } = recordingOps();
    const result = await syncGroupNotifications(42, diff, ops);

    // Exactly one call, for the provider the user actually ticked.
    expect(calls).toEqual([{ op: "attach", id: 3, groupId: 42 }]);
    expect(calls.filter((c) => c.id === DEFAULT_PROVIDER.id)).toHaveLength(0);
    expect(result.attached).toEqual([3]);
    expect(result.failures).toEqual([]);
  });

  test("create: ticking nothing at all issues no calls, even with default providers present", async () => {
    const providers = [DEFAULT_PROVIDER, provider(9, { is_default: true })];
    const baseline = initialGroupNotificationSelection(providers, null);
    const diff = diffNotificationLinks(baseline, []);

    expect(hasLinkChanges(diff)).toBe(false);

    const { calls, ops } = recordingOps();
    const result = await syncGroupNotifications(42, diff, ops);
    expect(calls).toEqual([]);
    expect(result).toEqual({ attached: [], detached: [], failures: [] });
  });

  test("edit: a default provider the user never ticked is not attached on save", async () => {
    const providers = [provider(1), DEFAULT_PROVIDER];
    const baseline = initialGroupNotificationSelection(providers, [1]); // only 1 is attached
    const diff = diffNotificationLinks(baseline, [1]); // user changed nothing

    expect(diff).toEqual({ attach: [], detach: [] });

    const { calls, ops } = recordingOps();
    await syncGroupNotifications(42, diff, ops);
    expect(calls).toEqual([]);
  });
});

describe("syncGroupNotifications", () => {
  test("detaches before it attaches, and passes (notificationId, groupId)", async () => {
    const { calls, ops } = recordingOps();
    const result = await syncGroupNotifications(
      9,
      { attach: [3], detach: [1] },
      ops,
    );

    expect(calls).toEqual([
      { op: "detach", id: 1, groupId: 9 },
      { op: "attach", id: 3, groupId: 9 },
    ]);
    expect(result).toEqual({ attached: [3], detached: [1], failures: [] });
  });

  test("a partial failure reports only what actually landed", async () => {
    const { calls, ops } = recordingOps({ attach: new Set([5]) });
    const result = await syncGroupNotifications(
      9,
      { attach: [4, 5], detach: [] },
      ops,
    );

    // It does not abort on the first failure — 4 landed, 5 did not.
    expect(calls).toHaveLength(2);
    expect(result.attached).toEqual([4]);
    expect(result.failures).toEqual([
      { id: 5, op: "attach", message: "attach 5 boom" },
    ]);
  });

  test("a failed detach is not reported as detached", async () => {
    const { ops } = recordingOps({ detach: new Set([1]) });
    const result = await syncGroupNotifications(
      9,
      { attach: [], detach: [1, 2] },
      ops,
    );

    expect(result.detached).toEqual([2]);
    expect(result.failures).toEqual([
      { id: 1, op: "detach", message: "detach 1 boom" },
    ]);
  });

  test("never throws — a failing op resolves into failures", async () => {
    const ops: NotificationLinkOps = {
      async attach() {
        throw new Error("network down");
      },
      async detach() {
        throw {}; // an error with no usable message
      },
    };
    const result = await syncGroupNotifications(
      9,
      { attach: [1], detach: [2] },
      ops,
    );
    expect(result.attached).toEqual([]);
    expect(result.detached).toEqual([]);
    expect(result.failures).toEqual([
      { id: 2, op: "detach", message: "Request failed" },
      { id: 1, op: "attach", message: "network down" },
    ]);
  });
});

describe("applySyncToBaseline", () => {
  test("a clean sync makes the baseline equal the selection", () => {
    const next = applySyncToBaseline([1, 2], {
      attached: [3],
      detached: [1],
      failures: [],
    });
    expect(next).toEqual([2, 3]);
  });

  test("a partial failure re-baselines to reality, so a retry only replays what failed", () => {
    // User wanted [2, 3] from [1, 2]: detach 1 (ok), attach 3 (failed).
    const baseline = [1, 2];
    const next = applySyncToBaseline(baseline, {
      attached: [],
      detached: [1],
      failures: [{ id: 3, op: "attach", message: "boom" }],
    });
    expect(next).toEqual([2]);

    // Retrying with the same intent now diffs to attach-3-only — no replay of the detach.
    expect(diffNotificationLinks(next, [2, 3])).toEqual({
      attach: [3],
      detach: [],
    });
  });

  test("a total failure leaves the baseline untouched", () => {
    const next = applySyncToBaseline([1, 2], {
      attached: [],
      detached: [],
      failures: [
        { id: 1, op: "detach", message: "boom" },
        { id: 3, op: "attach", message: "boom" },
      ],
    });
    expect(next).toEqual([1, 2]);
  });
});
