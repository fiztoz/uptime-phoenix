import { describe, expect, test } from "bun:test";
import { inspectBackupDocument } from "./backup-review";

describe("backup import review", () => {
  test("keeps legacy and current formats and counts records without exposing values", () => {
    const doc = {
      version: 2,
      monitors: [{ name: "private", config: { token: "secret" } }],
      probes: [{}],
      monitor_probe_assignments: [{}],
    };
    const review = inspectBackupDocument(doc);
    expect(review.version).toBe(2);
    expect(review.total).toBe(3);
    expect(review.counts.monitors).toBe(1);
    expect(JSON.stringify(review.counts)).not.toContain("secret");
    expect(review.doc).toBe(doc);
    expect(
      inspectBackupDocument({ version: 1, notifications: null }).total,
    ).toBe(0);
  });
  test("missing or zero version follows the server default; empty docs have zero objects", () => {
    expect(inspectBackupDocument({}).version).toBe(2);
    expect(inspectBackupDocument({ version: 0 }).total).toBe(0);
  });
  test("rejects unsupported versions and invalid collection shapes before confirmation", () => {
    for (const doc of [
      [],
      null,
      { version: 3 },
      { version: "2" },
      { monitors: {} },
      { monitors: [null] },
      { probes: ["secret"] },
    ]) {
      expect(() => inspectBackupDocument(doc)).toThrow();
    }
  });
});
