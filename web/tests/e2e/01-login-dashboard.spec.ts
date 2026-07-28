import { expect, test } from "@playwright/test";
import { loginViaUI } from "./helpers";

test("login renders the hydrated dashboard", async ({ page }) => {
  await loginViaUI(page);

  await expect(page).toHaveURL(/\/dashboard$/);
  await expect(page.getByRole("link", { name: "Dashboard" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Monitors" })).toBeVisible();
  await expect(page.getByText("Total Monitors")).toBeVisible();
  await expect(page.getByText("No monitors configured yet.")).toBeVisible();
});
