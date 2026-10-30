import { describe, expect, test } from "bun:test";
import en from "../../messages/en.json";
import th from "../../messages/th.json";

// GitHub #129: the dashboard banner renders "{downCount} <message>", choosing
// the singular key when exactly one monitor is down.
describe("dashboard down-monitor banner messages", () => {
  test('English singular agrees with "1 monitor"', () => {
    expect(`1 ${en.dashboard_banner_monitor_needs_attention}`).toBe(
      "1 monitor needs attention",
    );
    expect(`2 ${en.dashboard_banner_monitors_need_attention}`).toBe(
      "2 monitors need attention",
    );
  });

  test("Thai catalog carries a translation, not the English copy", () => {
    for (const key of [
      "dashboard_banner_monitor_needs_attention",
      "dashboard_banner_monitors_need_attention",
    ] as const) {
      expect(th[key]).not.toBe(en[key]);
      expect(th[key]).toContain("มอนิเตอร์");
    }
  });
});
