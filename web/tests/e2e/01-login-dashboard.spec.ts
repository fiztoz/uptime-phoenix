import { expect, test } from "@playwright/test";
import { loginViaUI } from "./helpers";

test("login renders the hydrated dashboard", async ({ page }) => {
  await loginViaUI(page);

  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByRole("link", { name: "Dashboard" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Monitors" })).toBeVisible();
  await expect(page.getByText("Total Monitors")).toBeVisible();
  await expect(page.getByText("No monitors configured yet.")).toBeVisible();

  await page.getByRole("button", { name: "Open wallboard" }).click();
  await expect(page.getByTestId("dashboard-wallboard")).toBeVisible();
  await expect(page.getByText("Live monitor wallboard")).toBeVisible();
  await expect(page.getByText("No monitors in this view.")).toBeVisible();

  await page.getByRole("button", { name: "Exit wallboard" }).click();
  await expect(page.getByTestId("dashboard-wallboard")).not.toBeVisible();

  await page.getByRole("button", { name: "Sort monitors" }).click();
  await page.getByRole("option", { name: "Status priority" }).click();
  await expect
    .poll(() => new URL(page.url()).searchParams.get("sort"))
    .toBe("status");

  const moveUpEarlier = page.getByRole("button", {
    name: "Move Up earlier",
  });
  for (let index = 0; index < 4; index++) await moveUpEarlier.click();
  await expect
    .poll(() => new URL(page.url()).searchParams.get("status_order"))
    .toBe("up,down,pending,maintenance,paused");
});
