import { expect, test } from "@playwright/test";
import { API_BASE, authToken, loginViaUI, uniqueName } from "./helpers";

test("folder navigation retains content while catalogs refresh", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  const name = uniqueName("Navigation folder");
  const created = await page.request.post(`${API_BASE}/api/monitor-groups`, {
    headers,
    data: { name, condition: "ignore" },
  });
  expect(created.ok()).toBeTruthy();
  const group = await created.json();
  const monitorResponse = await page.request.post(`${API_BASE}/api/monitors`, {
    headers,
    data: {
      name: uniqueName("Navigation monitor"),
      type: "http",
      active: false,
      interval: 60,
      timeout: 5,
      group_id: group.id,
      config: { url: `${API_BASE}/api/health/live` },
    },
  });
  expect(monitorResponse.ok()).toBeTruthy();
  const monitor = await monitorResponse.json();
  let release = () => {};
  let hold: Promise<void> | undefined;
  await page.route("**/api/monitor-groups", async (route) => {
    if (hold) await hold;
    await route.continue();
  });
  try {
    await page.reload();
    await expect(
      page.getByText(name, { exact: true }).filter({ visible: true }).first(),
    ).toBeVisible();
    hold = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.getByRole("link", { name: "Monitors", exact: true }).click();
    await expect(page).toHaveURL(/\/monitors$/);
    await expect(
      page.getByText(name, { exact: true }).filter({ visible: true }).first(),
    ).toBeVisible({
      timeout: 1500,
    });
    await expect(
      page.getByText("No monitors found. Create your first one."),
    ).not.toBeVisible();
    await page.getByRole("link", { name: "Dashboard", exact: true }).click();
    await expect(page).toHaveURL(/\/dashboard$/);
    await expect(
      page.getByText(name, { exact: true }).filter({ visible: true }).first(),
    ).toBeVisible({
      timeout: 1500,
    });
    await page.screenshot({
      animations: "disabled",
      path: "test-results/navigation-dashboard-desktop.png",
    });
    const refreshed = page.waitForResponse("**/api/monitor-groups");
    release();
    await refreshed;
    hold = undefined;

    // A hard reload drops the in-memory catalog. Hold the cold response
    // until the live monitor snapshot arrives: no false empty state.
    hold = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.goto("/monitors");
    await expect
      .poll(() =>
        page.evaluate(() => {
          const state = (
            window as Window & {
              __phoenixRealtime?: { hasMonitorSnapshot: boolean };
            }
          ).__phoenixRealtime;
          return state?.hasMonitorSnapshot ?? false;
        }),
      )
      .toBe(true);
    await expect(
      page.getByRole("status").filter({ hasText: "Loading" }).first(),
    ).toBeVisible();
    await expect(
      page.getByText("No monitors found. Create your first one."),
    ).not.toBeVisible();
    release();
    hold = undefined;
    await expect(
      page.getByText(name, { exact: true }).filter({ visible: true }).first(),
    ).toBeVisible();
    await page.screenshot({
      animations: "disabled",
      path: "test-results/navigation-monitors-desktop.png",
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(
      page.getByText(name, { exact: true }).filter({ visible: true }).first(),
    ).toBeVisible();
    await page.screenshot({
      animations: "disabled",
      path: "test-results/navigation-monitors-mobile.png",
    });
  } finally {
    release();
    await page.unrouteAll({ behavior: "wait" });
    await page.request.delete(`${API_BASE}/api/monitors/${monitor.id}`, {
      headers,
    });
    await page.request.delete(`${API_BASE}/api/monitor-groups/${group.id}`, {
      headers,
    });
  }
});
