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
    .locator("div.px-5.py-4")
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
