import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
} from "./helpers";

test("admin grants one monitor and the non-admin sees exactly that scope", async ({
  browser,
  page,
}) => {
  await loginViaUI(page);
  const adminToken = await authToken(page);
  const grantedName = uniqueName("Granted monitor");
  const hiddenName = uniqueName("Hidden monitor");
  await createHttpMonitorViaApi(page, adminToken, grantedName);
  await createHttpMonitorViaApi(page, adminToken, hiddenName);
  const username = uniqueName("viewer").toLowerCase();
  const password = "ViewerPass123!";

  await page.goto(`${BASE_URL}/settings#users`);
  await expect(page.getByTestId("users-section")).toBeVisible({
    timeout: 15_000,
  });
  await page.locator("#new-user-username").fill(username);
  await page.locator("#new-user-password").fill(password);
  await page.getByRole("button", { name: "Create user" }).click();

  const userRow = page.getByTestId("user-row").filter({ hasText: username });
  await expect(userRow).toBeVisible({ timeout: 15_000 });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page.locator("main").evaluate((element) => ({
        left: Math.round(element.getBoundingClientRect().left),
        width: Math.round(element.getBoundingClientRect().width),
        scrollWidth: document.documentElement.scrollWidth,
      })),
    )
    .toEqual({ left: 0, width: 390, scrollWidth: 390 });
  await userRow.getByTestId("user-access-btn").click();
  const editor = userRow.getByTestId("user-permission-editor");
  await expect(editor).toBeVisible();
  const picker = editor.getByRole("combobox", {
    name: "Search monitors to grant…",
  });
  await expect(picker).toBeEnabled({ timeout: 15_000 });
  await picker.fill(grantedName);
  await editor.getByRole("option", { name: new RegExp(grantedName) }).click();
  await editor.getByTestId("user-permission-save-btn").click();
  await expect(page.getByText("Permissions saved")).toBeVisible();

  const viewerContext = await browser.newContext();
  try {
    const viewerPage = await viewerContext.newPage();
    await loginViaUI(viewerPage, username, password);
    const viewerToken = await authToken(viewerPage);
    const response = await viewerPage.request.get(`${API_BASE}/api/monitors`, {
      headers: { Authorization: `Bearer ${viewerToken}` },
    });
    expect(response.ok()).toBeTruthy();
    const monitors = (await response.json()) as Array<{ name: string }>;
    expect(monitors.map((monitor) => monitor.name)).toEqual([grantedName]);

    await viewerPage.goto(`${BASE_URL}/monitors`);
    await expect(
      viewerPage.locator("table").getByText(grantedName, { exact: true }),
    ).toBeVisible();
    await expect(viewerPage.getByText(hiddenName, { exact: true })).toHaveCount(
      0,
    );
    await expect(
      viewerPage.getByRole("link", { name: "Status Pages", exact: true }),
    ).toHaveCount(0);
    await expect(
      viewerPage.getByRole("link", { name: "Backup", exact: true }),
    ).toHaveCount(0);
    await expect(
      viewerPage.getByRole("link", { name: "Notifications", exact: true }),
    ).toHaveCount(0);
    await expect(
      viewerPage.getByRole("link", { name: "Maintenance", exact: true }),
    ).toHaveCount(0);
  } finally {
    await viewerContext.close();
  }
});
