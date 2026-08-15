import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
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
  await dialog.locator("#cfg-json_query").fill("status");
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
      jsonQuery: "status",
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
