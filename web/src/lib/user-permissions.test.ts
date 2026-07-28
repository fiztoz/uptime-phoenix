/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  coveredMonitorIds,
  expandGroupIds,
  grantSummary,
  groupOptions,
  groupPath,
  normalizePermissions,
  permissionsEqual,
  type GrantableGroup,
  type GrantableMonitor,
} from "./user-permissions";
import type { GroupGrant } from "./api/users";

function group(
  id: number,
  name: string,
  parent_id: number | null = null,
): GrantableGroup {
  return { id, name, parent_id };
}

function monitor(
  id: number,
  name: string,
  group_id: number | null = null,
): GrantableMonitor {
  return { id, name, group_id };
}

// Platform ──┬── EU ──── Edge
//            └── US
// Standalone (no children)
const GROUPS: GrantableGroup[] = [
  group(1, "Platform"),
  group(2, "EU", 1),
  group(3, "Edge", 2),
  group(4, "US", 1),
  group(9, "Standalone"),
];

const MONITORS: GrantableMonitor[] = [
  monitor(10, "platform-api", 1),
  monitor(11, "eu-api", 2),
  monitor(12, "eu-edge-cdn", 3),
  monitor(13, "us-api", 4),
  monitor(14, "orphan", null),
  monitor(15, "standalone-svc", 9),
];

describe("groupPath", () => {
  test("renders the full ancestry path", () => {
    expect(groupPath(GROUPS, group(3, "Edge", 2))).toBe("Platform / EU / Edge");
  });

  test("a root group is just its name", () => {
    expect(groupPath(GROUPS, group(1, "Platform"))).toBe("Platform");
  });

  test("stops at a dangling parent rather than throwing", () => {
    const orphaned = group(7, "Lost", 999);
    expect(groupPath(GROUPS, orphaned)).toBe("Lost");
  });

  test("a parent cycle terminates instead of hanging", () => {
    // a -> b -> a. Rejected server-side; a stale cache must not spin forever.
    const cyclic = [group(20, "A", 21), group(21, "B", 20)];
    expect(groupPath(cyclic, cyclic[0])).toBe("B / A");
  });
});

/** A deep group grant — the default reach, and what every grant meant pre-011. */
const deep = (id: number): GroupGrant => ({
  group_id: id,
  include_descendants: true,
});
/** A shallow group grant: the folder and its own monitors, nothing below. */
const shallow = (id: number): GroupGrant => ({
  group_id: id,
  include_descendants: false,
});

describe("expandGroupIds", () => {
  test("a deep grant pulls in every descendant, at any depth", () => {
    expect(
      [...expandGroupIds(GROUPS, [deep(1)])].sort((a, b) => a - b),
    ).toEqual([1, 2, 3, 4]);
  });

  test("a shallow grant reaches the folder and stops", () => {
    expect([...expandGroupIds(GROUPS, [shallow(1)])]).toEqual([1]);
  });

  test("granting a subgroup does not reach back up to its parent", () => {
    expect(
      [...expandGroupIds(GROUPS, [deep(2)])].sort((a, b) => a - b),
    ).toEqual([2, 3]);
  });

  test("a leaf group expands to itself", () => {
    expect([...expandGroupIds(GROUPS, [deep(9)])]).toEqual([9]);
  });

  test("a group ID that no longer exists is dropped", () => {
    expect([...expandGroupIds(GROUPS, [deep(404)])]).toEqual([]);
  });

  test("overlapping grants are not double-counted", () => {
    expect(
      [...expandGroupIds(GROUPS, [deep(1), deep(2), deep(3)])].sort(
        (a, b) => a - b,
      ),
    ).toEqual([1, 2, 3, 4]);
  });

  test("a shallow grant cannot narrow a deep one that already covers it", () => {
    // Grants are additive and there is no deny — same rule as the server. Someone
    // will try to use a shallow grant to punch a hole in a subtree; it does not
    // work, and the preview must not pretend otherwise.
    expect(
      [...expandGroupIds(GROUPS, [deep(1), shallow(2)])].sort((a, b) => a - b),
    ).toEqual([1, 2, 3, 4]);
  });

  test("no grants reach nothing", () => {
    expect(expandGroupIds(GROUPS, []).size).toBe(0);
  });
});

describe("coveredMonitorIds", () => {
  test("a deep group grants every monitor inside it, recursively", () => {
    // This is the load-bearing claim the UI makes to the admin. If it is wrong,
    // the admin hands out access they did not think they were handing out.
    expect(
      [...coveredMonitorIds(GROUPS, MONITORS, [deep(1)])].sort((a, b) => a - b),
    ).toEqual([10, 11, 12, 13]);
  });

  test("a shallow group grants only the monitors filed directly in it", () => {
    // The other half of the claim, and the one an admin is relying on when they
    // untick "include subfolders". Monitor 10 sits in group 1 itself; 11/12/13
    // are all further down and must stay out.
    expect([...coveredMonitorIds(GROUPS, MONITORS, [shallow(1)])]).toEqual([
      10,
    ]);
  });

  test("a mid-tree group covers only its own subtree", () => {
    expect(
      [...coveredMonitorIds(GROUPS, MONITORS, [deep(2)])].sort((a, b) => a - b),
    ).toEqual([11, 12]);
  });

  test("an ungrouped monitor is never covered by any group grant", () => {
    const covered = coveredMonitorIds(GROUPS, MONITORS, [deep(1), deep(9)]);
    expect(covered.has(14)).toBe(false);
  });

  test("no group grants cover nothing", () => {
    expect(coveredMonitorIds(GROUPS, MONITORS, []).size).toBe(0);
  });
});

describe("grantSummary", () => {
  test("counts direct and via-group monitors without double-counting", () => {
    const s = grantSummary(GROUPS, MONITORS, {
      monitor_ids: [14],
      groups: [deep(2)],
    });
    expect(s.direct).toBe(1); // orphan
    expect(s.viaGroups).toBe(2); // eu-api, eu-edge-cdn
    expect(s.total).toBe(3);
    expect(s.groups).toBe(1);
    expect(s.redundantDirect).toBe(0);
  });

  test("a shallow grant reports the smaller number it actually hands over", () => {
    // The summary is the only place an admin sees what unticking "include
    // subfolders" bought them. Deep EU is 2 monitors; shallow EU is 1.
    const s = grantSummary(GROUPS, MONITORS, {
      monitor_ids: [],
      groups: [shallow(2)],
    });
    expect(s.total).toBe(1); // eu-api only — eu-edge-cdn is in the Edge subfolder
    expect(s.viaGroups).toBe(1);
  });

  test("flags a direct grant that a granted group already covers", () => {
    // eu-api (11) sits inside EU (2). Granting both is legal — the server unions
    // them — but revoking the group would NOT revoke monitor 11, and the admin
    // deserves to know that before they are surprised by it.
    const s = grantSummary(GROUPS, MONITORS, {
      monitor_ids: [11],
      groups: [deep(2)],
    });
    expect(s.direct).toBe(1);
    expect(s.redundantDirect).toBe(1);
    expect(s.viaGroups).toBe(1); // only eu-edge-cdn is *added* by the group
    expect(s.total).toBe(2);
  });

  test("an empty grant sees zero monitors", () => {
    const s = grantSummary(GROUPS, MONITORS, { monitor_ids: [], groups: [] });
    expect(s.total).toBe(0);
    expect(s.direct).toBe(0);
    expect(s.viaGroups).toBe(0);
  });

  test("a monitor grant for a monitor that no longer exists is not counted", () => {
    const s = grantSummary(GROUPS, MONITORS, {
      monitor_ids: [9999],
      groups: [],
    });
    expect(s.direct).toBe(0);
    expect(s.total).toBe(0);
  });
});

describe("normalizePermissions", () => {
  test("dedupes and sorts both sets", () => {
    expect(
      normalizePermissions({
        monitor_ids: [3, 1, 3],
        groups: [deep(2), deep(2)],
      }),
    ).toEqual({
      monitor_ids: [1, 3],
      groups: [deep(2)],
    });
  });

  test("a duplicated group keeps its FIRST reach, matching the server", () => {
    // Contradictory input the UI cannot produce, but a hand-rolled API call can.
    // Both sides must resolve it identically or the dirty check disagrees with
    // what was stored.
    expect(
      normalizePermissions({ monitor_ids: [], groups: [shallow(2), deep(2)] }),
    ).toEqual({
      monitor_ids: [],
      groups: [shallow(2)],
    });
  });
});

describe("permissionsEqual", () => {
  test("order and duplicates do not make a draft dirty", () => {
    expect(
      permissionsEqual(
        { monitor_ids: [2, 1, 1], groups: [deep(5)] },
        { monitor_ids: [1, 2], groups: [deep(5)] },
      ),
    ).toBe(true);
  });

  test("an added monitor makes it dirty", () => {
    expect(
      permissionsEqual(
        { monitor_ids: [1], groups: [] },
        { monitor_ids: [1, 2], groups: [] },
      ),
    ).toBe(false);
  });

  test("an added group makes it dirty", () => {
    expect(
      permissionsEqual(
        { monitor_ids: [], groups: [] },
        { monitor_ids: [], groups: [deep(1)] },
      ),
    ).toBe(false);
  });

  test("flipping a group deep -> shallow makes it dirty", () => {
    // Same folder, same id — only the reach moved. An id-only comparison would
    // call this clean, grey out Save, and silently strand the admin's change.
    expect(
      permissionsEqual(
        { monitor_ids: [], groups: [deep(1)] },
        { monitor_ids: [], groups: [shallow(1)] },
      ),
    ).toBe(false);
  });

  test("a removed grant makes it dirty — a REPLACE-SET must be able to shrink", () => {
    expect(
      permissionsEqual(
        { monitor_ids: [1, 2], groups: [] },
        { monitor_ids: [1], groups: [] },
      ),
    ).toBe(false);
  });

  test("two empty sets are equal", () => {
    expect(
      permissionsEqual(
        { monitor_ids: [], groups: [] },
        { monitor_ids: [], groups: [] },
      ),
    ).toBe(true);
  });
});

describe("groupOptions", () => {
  test("each group carries its path, sorted so a parent precedes its children", () => {
    expect(groupOptions(GROUPS)).toEqual([
      { id: 1, path: "Platform" },
      { id: 2, path: "Platform / EU" },
      { id: 3, path: "Platform / EU / Edge" },
      { id: 4, path: "Platform / US" },
      { id: 9, path: "Standalone" },
    ]);
  });
});
