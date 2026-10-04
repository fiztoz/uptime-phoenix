import { expect, test } from "@playwright/test";
import { BASE_URL } from "./helpers";

// Presentation contract: actual API authorization and overall aggregation are
// exercised by backend integration tests; this fixture proves disclosure in UI.
test("public coverage is overall, nullable, and safe across all page densities", async ({
  page,
}) => {
  const slug = "coverage-fixture";
  let density = "full";
  let coverage: number | null = 42.5;
  const requested: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/api/")) requested.push(request.url());
  });
  await page.route(`**/api/status/${slug}`, (route) =>
    route.fulfill({
      json: {
        status_page: {
          id: 1,
          title: "Public coverage",
          slug,
          description: "",
          theme: "light",
          published: true,
          dashboard_style: density,
          has_access: false,
          created_at: "2026-09-28T00:00:00Z",
          updated_at: "2026-09-28T00:00:00Z",
        },
        subscriptions_available: false,
        monitors: [
          {
            id: 1,
            name: "Checkout",
            type: "http",
            status: "unknown",
            uptime_percent: null,
            coverage_percent: coverage,
            uptime_data: [],
            uptime_history: { monthly: [], quarterly: [] },
            chart: null,
          },
        ],
        incidents: [],
      },
    }),
  );
  await page.setViewportSize({ width: 390, height: 844 });
  for (const view of ["full", "grid", "pills"]) {
    density = view;
    await page.goto(`${BASE_URL}/status/${slug}`);
    await expect(page.getByTestId("public-coverage")).toContainText(
      "24h coverage 42.5%",
    );
    await expect(
      page.getByText("Unknown", { exact: true }).first(),
    ).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
  }
  coverage = 0;
  await page.reload();
  await expect(page.getByTestId("public-coverage")).toContainText("0.0%");
  coverage = null;
  await page.reload();
  await expect(page.getByTestId("public-coverage")).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Region", exact: true }),
  ).toHaveCount(0);
  expect(
    requested.filter(
      (url) => url.includes("/probes") || url.includes("/probe-alerts"),
    ),
  ).toEqual([]);
  expect(
    await page.evaluate(() => localStorage.getItem("phoenix_jwt")),
  ).toBeNull();
});
