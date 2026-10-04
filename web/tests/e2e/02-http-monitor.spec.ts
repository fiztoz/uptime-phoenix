import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
  type MonitorView,
} from "./helpers";

test("create HTTP monitor persists advanced settings and records a heartbeat", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitorName = uniqueName("HTTP assurance");

  await page.goto(`${BASE_URL}/monitors`);
  await page.getByRole("button", { name: "Add Monitor" }).click();
  const dialog = page.getByRole("dialog", { name: "Create Monitor" });
  await expect(dialog).toBeVisible();

  await dialog.locator("#monitor-name").fill(monitorName);
  await dialog.locator("#cfg-url").fill(`${BASE_URL}/api/health/live`);
  await dialog.locator("#monitor-interval").fill("10");
  await dialog.locator("#monitor-timeout").fill("5");
  await dialog.locator("#cfg-json_query_syntax").click();
  await page.getByRole("option", { name: "JSONPath ($...)" }).click();
  await dialog.locator("#cfg-json_query").fill("$.status");
  await dialog.locator("#cfg-json_operator").click();
  await page.getByRole("option", { name: "Value equals" }).click();
  await dialog.locator("#cfg-expected_value").fill("alive");
  await dialog.locator("#monitor-retry-interval").fill("7");
  await dialog.locator("#monitor-max-retries").fill("2");
  await dialog.locator("#monitor-resend-interval").fill("3");
  await dialog.locator("#monitor-tls-ignore").check();
  await dialog.getByRole("button", { name: "Create Monitor" }).click();

  await expect(dialog).not.toBeVisible({ timeout: 15_000 });
  await expect(
    page.locator("table").getByText(monitorName, { exact: true }),
  ).toBeVisible();

  let monitor: MonitorView | undefined;
  await expect
    .poll(
      async () => {
        const response = await page.request.get(`${API_BASE}/api/monitors`, {
          headers: { Authorization: `Bearer ${token}` },
        });
        expect(response.ok()).toBeTruthy();
        const monitors = (await response.json()) as MonitorView[];
        monitor = monitors.find((candidate) => candidate.name === monitorName);
        return monitor
          ? {
              retry: monitor.retry_interval,
              maxRetries: monitor.max_retries,
              resend: monitor.resend_interval,
              tlsIgnore: monitor.tls_ignore,
              jsonQuery: monitor.config?.json_query,
              jsonQuerySyntax: monitor.config?.json_query_syntax,
              jsonOperator: monitor.config?.json_operator,
              expectedValue: monitor.config?.expected_value,
            }
          : null;
      },
      { timeout: 15_000 },
    )
    .toEqual({
      retry: 7,
      maxRetries: 2,
      resend: 3,
      tlsIgnore: true,
      jsonQuery: "$.status",
      jsonQuerySyntax: "jsonpath",
      jsonOperator: "equals",
      expectedValue: "alive",
    });

  if (!monitor) throw new Error("created monitor not returned by API");
  await expect
    .poll(
      async () => {
        const response = await page.request.get(
          `${API_BASE}/api/monitors/${monitor!.id}/heartbeats?hours=1`,
          { headers: { Authorization: `Bearer ${token}` } },
        );
        if (!response.ok()) return [];
        const heartbeats = (await response.json()) as Array<{ status: string }>;
        return heartbeats.map((heartbeat) => heartbeat.status);
      },
      { timeout: 20_000, intervals: [250, 500, 1_000] },
    )
    .toContain("up");
});

test("response chart reacts to data and viewport changes", async ({ page }) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("Chart"),
  );
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await page.route(
    `**/api/monitors/${monitor.id}/**/chart?*`,
    async (route) => {
      const hours = new URL(route.request().url()).searchParams.get("hours");
      const value = hours === "1" ? 180 : 60;
      await route.fulfill({
        json: {
          buckets: [
            {
              time: new Date(Date.now() - 3_000_000).toISOString(),
              min: 10,
              avg: 20,
              max: 30,
            },
            {
              time: new Date(Date.now() - 300_000).toISOString(),
              min: value - 10,
              avg: value,
              max: value + 10,
            },
          ],
          downtime_intervals: [],
          unknown_intervals: [],
          latency_available: true,
        },
      });
    },
  );
  await page.setViewportSize({ width: 1280, height: 900 });
  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
  const chart = page.getByTestId("response-chart");
  const line = chart.locator(".path-line");
  await expect(line).toHaveAttribute("d", /^M.+L/, { timeout: 10_000 });
  await expect(chart.locator("circle")).toHaveCount(2);
  const initialPath = await line.getAttribute("d");
  await page.setViewportSize({ width: 1000, height: 900 });
  await expect.poll(() => line.getAttribute("d")).not.toBe(initialPath);
  await page.getByLabel("Chart time range").click();
  await page.getByRole("option", { name: "1h", exact: true }).click();
  const point = chart.locator("circle").last();
  await expect
    .poll(async () => {
      const bounds = await point.boundingBox();
      if (bounds)
        await page.mouse.move(
          bounds.x + bounds.width / 2,
          bounds.y + bounds.height / 2,
        );
      return chart.locator(".chart-tooltip").textContent();
    })
    .toContain("Avg 180 ms");
  expect(await line.getAttribute("d")).not.toMatch(/NaN|Infinity/);
  expect(errors).toEqual([]);
});
