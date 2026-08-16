import { describe, expect, test } from "bun:test";
import { cardUsesSignals, parseDashboardCardBody } from "./dashboard-card";

describe("dashboard card body", () => {
  test("unknown values fall back to response", () => {
    expect(parseDashboardCardBody(undefined)).toBe("response");
    expect(parseDashboardCardBody(null)).toBe("response");
    expect(parseDashboardCardBody("capacity")).toBe("response");
    expect(parseDashboardCardBody("signals")).toBe("signals");
  });

  test("capacity meters replace the graph only when signals exist", () => {
    expect(cardUsesSignals("response", 2)).toBe(false);
    expect(cardUsesSignals("signals", 0)).toBe(false);
    expect(cardUsesSignals("signals", 1)).toBe(true);
  });
});
