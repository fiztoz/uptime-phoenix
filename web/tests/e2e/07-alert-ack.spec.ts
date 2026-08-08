import { expect, test, type Page } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  loginViaUI,
  uniqueName,
} from "./helpers";

const STUB_BASE = "http://127.0.0.1:3101";

interface AlertView {
  id: number;
  monitor_id: number;
  status: "firing" | "acked" | "resolved";
  message: string;
  acked_by_user_id?: number | null;
}

async function createFailingMonitor(
  page: Page,
  token: string,
  name: string,
): Promise<number> {
  const response = await page.request.post(`${API_BASE}/api/monitors`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      name,
      type: "tcp",
      active: true,
      interval: 10,
      timeout: 2,
      retry_interval: 10,
      max_retries: 0,
      resend_interval: 0,
      config: { hostname: "127.0.0.1", port: 9 },
    },
  });
  expect(response.ok(), await response.text()).toBeTruthy();
  return ((await response.json()) as { id: number }).id;
}

async function waitForOpenAlert(
  page: Page,
  token: string,
  monitorId: number,
): Promise<AlertView> {
  let current: AlertView[] = [];
  await expect
    .poll(
      async () => {
        const response = await page.request.get(
          `${API_BASE}/api/alerts?open=1&monitor_id=${monitorId}`,
          { headers: { Authorization: `Bearer ${token}` } },
        );
        if (!response.ok()) return 0;
        current = (await response.json()) as AlertView[];
        return current.length;
      },
      {
        timeout: 60_000,
        message: "the failing monitor should persist one open alert",
      },
    )
    .toBe(1);
  return current[0];
}

test("admin acknowledges an alert in the UI and the effect is persisted", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitorName = uniqueName("Admin ack target");
  const monitorId = await createFailingMonitor(page, token, monitorName);
  const firing = await waitForOpenAlert(page, token, monitorId);
  expect(firing.status).toBe("firing");

  await page.goto(`${BASE_URL}/alerts`);
  const row = page
    .getByTestId("alert-row")
    .filter({ hasText: monitorName })
    .first();
  await expect(row).toBeVisible({ timeout: 30_000 });
  const ackResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(`/api/alerts/${firing.id}/ack`),
  );
  await row.getByRole("button", { name: "Acknowledge" }).click();
  expect((await ackResponse).ok()).toBeTruthy();
  await expect(row.getByText("Acknowledged", { exact: false })).toBeVisible();

  const persisted = await waitForOpenAlert(page, token, monitorId);
  expect(persisted.status).toBe("acked");
  expect(persisted.acked_by_user_id).toBeGreaterThan(0);
  expect(persisted).not.toHaveProperty("ack_token");
});

test("admin acknowledges multiple selected alerts in one bulk action", async ({
  page,
}) => {
  test.setTimeout(120_000);
  await loginViaUI(page);
  const token = await authToken(page);
  const firstName = uniqueName("Bulk ack target A");
  const secondName = uniqueName("Bulk ack target B");
  const firstMonitorID = await createFailingMonitor(page, token, firstName);
  const secondMonitorID = await createFailingMonitor(page, token, secondName);
  const [firstAlert, secondAlert] = await Promise.all([
    waitForOpenAlert(page, token, firstMonitorID),
    waitForOpenAlert(page, token, secondMonitorID),
  ]);

  await page.goto(`${BASE_URL}/alerts`);
  const firstRow = page
    .getByTestId("alert-row")
    .filter({ hasText: firstName })
    .first();
  const secondRow = page
    .getByTestId("alert-row")
    .filter({ hasText: secondName })
    .first();
  await expect(firstRow).toBeVisible({ timeout: 30_000 });
  await expect(secondRow).toBeVisible({ timeout: 30_000 });

  await firstRow
    .getByRole("checkbox", { name: `Select alert for ${firstName}` })
    .check();
  await secondRow
    .getByRole("checkbox", { name: `Select alert for ${secondName}` })
    .check();
  await expect(page.getByText("2 selected", { exact: true })).toBeVisible();

  const firstAckResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(`/api/alerts/${firstAlert.id}/ack`),
  );
  const secondAckResponse = page.waitForResponse(
    (response) =>
      response.request().method() === "POST" &&
      response.url().endsWith(`/api/alerts/${secondAlert.id}/ack`),
  );
  await page.getByRole("button", { name: "Acknowledge selected" }).click();
  const responses = await Promise.all([firstAckResponse, secondAckResponse]);
  expect(responses.every((response) => response.ok())).toBeTruthy();

  await expect(
    firstRow.getByText("Acknowledged", { exact: false }),
  ).toBeVisible();
  await expect(
    secondRow.getByText("Acknowledged", { exact: false }),
  ).toBeVisible();
  await expect(
    page.getByText("2 alerts acknowledged", { exact: true }),
  ).toBeVisible();

  const [persistedFirst, persistedSecond] = await Promise.all([
    waitForOpenAlert(page, token, firstMonitorID),
    waitForOpenAlert(page, token, secondMonitorID),
  ]);
  expect(persistedFirst.status).toBe("acked");
  expect(persistedSecond.status).toBe("acked");
  expect(persistedFirst.acked_by_user_id).toBeGreaterThan(0);
  expect(persistedSecond.acked_by_user_id).toBeGreaterThan(0);
});

test("admin filters alerts by search and status", async ({ page }) => {
  test.setTimeout(120_000);
  await loginViaUI(page);
  const token = await authToken(page);
  const firstName = uniqueName("Filter target A");
  const secondName = uniqueName("Filter target B");
  const firstMonitorID = await createFailingMonitor(page, token, firstName);
  const secondMonitorID = await createFailingMonitor(page, token, secondName);
  await Promise.all([
    waitForOpenAlert(page, token, firstMonitorID),
    waitForOpenAlert(page, token, secondMonitorID),
  ]);

  await page.goto(`${BASE_URL}/alerts`);
  const firstRow = page
    .getByTestId("alert-row")
    .filter({ hasText: firstName })
    .first();
  const secondRow = page
    .getByTestId("alert-row")
    .filter({ hasText: secondName })
    .first();
  await expect(firstRow).toBeVisible({ timeout: 30_000 });
  await expect(secondRow).toBeVisible({ timeout: 30_000 });

  await page
    .getByRole("textbox", { name: "Search by monitor or message…" })
    .fill(firstName);
  await expect(firstRow).toBeVisible();
  await expect(secondRow).not.toBeVisible();
  await expect(page.getByText(/of \d+ alerts/)).toBeVisible();

  await page.getByRole("button", { name: "Clear search" }).click();
  await expect(secondRow).toBeVisible();

  const firingList = page.waitForResponse(
    (response) =>
      response.request().method() === "GET" &&
      response.url().includes("/api/alerts?") &&
      response.url().includes("status=firing") &&
      response.ok(),
  );
  await page.getByRole("button", { name: "Firing", exact: true }).click();
  await firingList;
  await expect(firstRow).toBeVisible({ timeout: 30_000 });
  await expect(secondRow).toBeVisible();

  await firstRow.getByRole("button", { name: "Acknowledge" }).click();
  await expect(firstRow).not.toBeVisible({ timeout: 15_000 });
  await expect(secondRow).toBeVisible();
});

test("anonymous deep link acknowledges the exact alert from a real notification", async ({
  browser,
  page,
}) => {
  test.setTimeout(120_000);
  await loginViaUI(page);
  const token = await authToken(page);
  const reset = await page.request.post(`${STUB_BASE}/reset`);
  expect(reset.ok()).toBeTruthy();

  const channelResponse = await page.request.post(
    `${API_BASE}/api/notifications`,
    {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        name: uniqueName("Ack link webhook"),
        type: "webhook",
        active: true,
        is_default: true,
        config: { url: `${STUB_BASE}/notify` },
      },
    },
  );
  expect(channelResponse.ok(), await channelResponse.text()).toBeTruthy();

  const monitorName = uniqueName("Public ack target");
  const monitorId = await createFailingMonitor(page, token, monitorName);
  const firing = await waitForOpenAlert(page, token, monitorId);

  await expect
    .poll(
      async () => {
        const response = await page.request.get(`${STUB_BASE}/count`);
        if (!response.ok()) return 0;
        return ((await response.json()) as { count: number }).count;
      },
      {
        timeout: 30_000,
        message: "the DOWN notification should reach the webhook sink",
      },
    )
    .toBeGreaterThanOrEqual(1);

  const payloadResponse = await page.request.get(`${STUB_BASE}/last`);
  expect(payloadResponse.ok()).toBeTruthy();
  const payload = (await payloadResponse.json()) as {
    message: string;
    monitor: { id: number };
  };
  expect(payload.monitor.id).toBe(monitorId);
  const ackURL = payload.message.match(/Acknowledge: (https?:\/\/\S+)/)?.[1];
  expect(
    ackURL,
    `notification did not carry an acknowledgement link: ${payload.message}`,
  ).toBeTruthy();

  const anonymous = await browser.newContext();
  try {
    const publicPage = await anonymous.newPage();
    await publicPage.goto(ackURL!);
    await expect(
      publicPage.getByRole("heading", { name: "Alert acknowledged" }),
    ).toBeVisible({ timeout: 15_000 });
    await expect(
      publicPage.getByText(monitorName, { exact: false }),
    ).toBeVisible();
    expect(
      await publicPage.evaluate(() => localStorage.getItem("phoenix_jwt")),
    ).toBeNull();
  } finally {
    await anonymous.close();
  }

  const persisted = await waitForOpenAlert(page, token, monitorId);
  expect(persisted.id).toBe(firing.id);
  expect(persisted.status).toBe("acked");
  expect(persisted.acked_by_user_id ?? null).toBeNull();
  expect(persisted).not.toHaveProperty("ack_token");
});
