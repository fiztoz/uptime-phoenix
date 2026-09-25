/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import {
  createProjectionInvalidator,
  decideProjectionVersion,
} from "./projection-version";

describe("decideProjectionVersion", () => {
  test("keeps unversioned local checks applicable", () => {
    expect(decideProjectionVersion(undefined, 0)).toBe("unversioned");
    expect(decideProjectionVersion(4, Number.NaN)).toBe("unversioned");
  });

  test("advances once and then ignores an older replay", () => {
    expect(decideProjectionVersion(undefined, 2)).toBe("newer");
    expect(decideProjectionVersion(2, 2)).toBe("current");
    expect(decideProjectionVersion(2, 1)).toBe("stale");
    expect(decideProjectionVersion(2, 3)).toBe("newer");
  });
});

describe("createProjectionInvalidator", () => {
  test("clears navigation caches once per scheduled turn", () => {
    const queued: Array<() => void> = [];
    let clears = 0;
    const invalidate = createProjectionInvalidator(
      () => {
        clears++;
      },
      (run) => queued.push(run),
    );
    invalidate();
    invalidate();
    expect(queued).toHaveLength(1);
    expect(clears).toBe(0);
    queued[0]?.();
    expect(clears).toBe(1);
    invalidate();
    expect(queued).toHaveLength(2);
  });
});
