/// <reference types="bun-types" />
import { describe, expect, test } from "bun:test";
import { computeOverall, footerCopy, severityOf } from "./status-page-health";

describe("computeOverall", () => {
  test("is operational with no downs or active incidents", () => {
    expect(computeOverall([{ status: "up" }], [])).toBe("operational");
  });

  test("is degraded for active info and warning incidents", () => {
    expect(computeOverall([{ status: "up" }], [{ style: "info" }])).toBe(
      "degraded",
    );
    expect(computeOverall([{ status: "up" }], [{ style: "warning" }])).toBe(
      "degraded",
    );
  });

  test("is an outage for a danger incident even when every monitor is up", () => {
    expect(computeOverall([{ status: "up" }], [{ style: "danger" }])).toBe(
      "outage",
    );
  });

  test("is an outage when any monitor is down", () => {
    expect(computeOverall([{ status: "up" }, { status: "down" }], [])).toBe(
      "outage",
    );
  });

  test("treats missing and unknown incident styles as informational", () => {
    expect(severityOf(undefined).style).toBe("info");
    expect(severityOf("").style).toBe("info");
    expect(severityOf("legacy").style).toBe("info");
    expect(computeOverall([{ status: "up" }], [{}])).toBe("degraded");
  });
});

describe("footerCopy", () => {
  test("keeps operator-provided copy", () => {
    expect(footerCopy(" Planned maintenance tonight ", "outage")).toBe(
      "Planned maintenance tonight",
    );
  });

  test("never claims normal operation during degraded health or an outage", () => {
    expect(footerCopy(undefined, "operational")).toBe(
      "All systems operational",
    );
    expect(footerCopy(undefined, "degraded")).toBe(
      "Some systems are experiencing degraded performance",
    );
    expect(footerCopy(undefined, "outage")).toBe(
      "Some systems are currently unavailable",
    );
  });
});
