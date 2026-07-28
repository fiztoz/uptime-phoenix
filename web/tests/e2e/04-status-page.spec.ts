import { expect, test } from "@playwright/test";
import {
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
} from "./helpers";

test("create status page, attach monitor, and render it without authentication", async ({
  browser,
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitorName = uniqueName("Public monitor");
  await createHttpMonitorViaApi(page, token, monitorName);
  const title = uniqueName("Public status");
  const slug = uniqueName("public").toLowerCase();

  await page.goto(`${BASE_URL}/status-pages`);
  await page.getByRole("button", { name: "New Status Page" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Create Status Page" });
  await dialog.locator("#sp-title").fill(title);
  await dialog.locator("#sp-slug").fill(slug);
  await dialog.locator("#sp-dashboard-style").click();
  await page.getByRole("option", { name: "Service grid", exact: true }).click();
  await dialog.locator("#show-sla-target").check();
  await dialog.locator("#sla-target").fill("99.9");
  await dialog.getByRole("button", { name: "Create" }).click();
  await page.waitForURL(/\/status-pages\/\d+$/, { timeout: 15_000 });

  const picker = page.getByRole("combobox", { name: "Add a monitor…" });
  await expect(picker).toBeEnabled({ timeout: 15_000 });
  await picker.fill(monitorName);
  await page.getByRole("option", { name: new RegExp(monitorName) }).click();
  await expect(page.getByText(monitorName, { exact: true })).toBeVisible({
    timeout: 15_000,
  });

  const anonymous = await browser.newContext();
  try {
    const publicPage = await anonymous.newPage();
    await publicPage.setViewportSize({ width: 390, height: 844 });
    await publicPage.goto(`${BASE_URL}/${slug}`);
    await expect(
      publicPage.getByRole("heading", { name: title }),
    ).toBeVisible();
    await expect(
      publicPage.getByText(monitorName, { exact: true }),
    ).toBeVisible({ timeout: 15_000 });
    await publicPage.getByRole("link", { name: "View uptime history" }).click();
    await expect(
      publicPage.getByRole("heading", { name: "Component uptime" }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(publicPage.getByText("Public SLA target")).toBeVisible();
    await expect(publicPage.getByText("99.9%", { exact: true })).toBeVisible();
    await expect(
      publicPage.getByRole("rowheader", { name: new RegExp(monitorName) }),
    ).toBeVisible();
    await publicPage.getByRole("button", { name: "Quarterly" }).click();
    const quarterHeaders = publicPage.getByRole("columnheader", {
      name: /Q[1-4] 20\d{2}/,
    });
    await expect(quarterHeaders).toHaveCount(4);
    await expect(quarterHeaders.first()).toBeVisible();
    expect(
      await publicPage.evaluate(() => localStorage.getItem("phoenix_jwt")),
    ).toBeNull();
    expect(
      await publicPage.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
  } finally {
    await anonymous.close();
  }
});
