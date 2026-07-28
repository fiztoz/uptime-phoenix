import { expect, test } from "@playwright/test";
import { BASE_URL, loginViaUI, uniqueName } from "./helpers";

test("create notification and report a successful test send honestly", async ({
  page,
}) => {
  await loginViaUI(page);
  const notificationName = uniqueName("Webhook assurance");

  await page.goto(`${BASE_URL}/notifications`);
  await page.getByRole("button", { name: "Add Notification" }).click();
  const dialog = page.getByRole("dialog", { name: "Add Notification" });
  await expect(dialog).toBeVisible();

  await dialog.locator("#notif-name").fill(notificationName);
  await dialog.locator("#notif-type").click();
  await page.getByRole("option", { name: "Webhook", exact: true }).click();
  await dialog.locator("#notif-url").fill("http://127.0.0.1:3101/notify");
  await dialog.getByRole("button", { name: "Create" }).click();

  const card = page
    .locator("div.rounded-xl")
    .filter({ hasText: notificationName })
    .first();
  await expect(card).toBeVisible({ timeout: 15_000 });
  const testResponse = page.waitForResponse(
    (response) =>
      /\/api\/notifications\/\d+\/test$/.test(response.url()) &&
      response.request().method() === "POST",
  );
  await card.getByRole("button", { name: "Test" }).click();
  expect((await testResponse).ok()).toBeTruthy();
  await expect(
    page.getByText(`Test sent to ${notificationName}`),
  ).toBeVisible();
});
