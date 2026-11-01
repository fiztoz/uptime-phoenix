import { describe, expect, test } from "bun:test";
import en from "../../messages/en.json";
import th from "../../messages/th.json";
import { alertDeliveryMessageKey } from "./alert-delivery-text";

// GitHub #175: the probe panel named per-region delivery whatever mode was
// saved, and channel configuration said nothing about delivery scope.
describe("alert delivery scope text", () => {
  test("each saved mode has its own sentence", () => {
    expect(alertDeliveryMessageKey("regional")).toBe(
      "probes_delivery_regional",
    );
    expect(alertDeliveryMessageKey("aggregate")).toBe(
      "probes_delivery_aggregate",
    );
    expect(alertDeliveryMessageKey("both")).toBe("probes_delivery_both");
  });

  test("an absent legacy mode reads as regional", () => {
    expect(alertDeliveryMessageKey("")).toBe("probes_delivery_regional");
    expect(alertDeliveryMessageKey(undefined)).toBe("probes_delivery_regional");
  });

  test("the sentences differ and exist in both catalogs", () => {
    const keys = [
      "probes_delivery_regional",
      "probes_delivery_aggregate",
      "probes_delivery_both",
      "notifications_delivery_scope_hint",
    ] as const;
    expect(new Set(keys.map((k) => en[k])).size).toBe(keys.length);
    for (const key of keys) {
      expect(en[key]).toBeTruthy();
      expect(th[key]).toBeTruthy();
      expect(th[key]).not.toBe(en[key]);
    }
  });

  test("the channel hint names how to tell the alerts apart", () => {
    for (const catalog of [en, th]) {
      expect(catalog.notifications_delivery_scope_hint).toContain(
        "delivery_scope",
      );
      expect(catalog.notifications_delivery_scope_hint).toContain("AGGREGATE");
    }
  });
});
