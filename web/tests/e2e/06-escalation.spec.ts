import { expect, test, type Page } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  loginViaUI,
  uniqueName,
} from "./helpers";

const STUB_BASE = "http://127.0.0.1:3101";

/**
 * F2.3: DOWN → escalation pending → acknowledge → no later step.
 *
 * The load-bearing assertion is the webhook stub's delivery COUNT, not a status
 * code. A 200 from the ack endpoint proves nothing about whether the ladder
 * stopped; only the count refusing to move does.
 *
 * The server under test runs with ESCALATION_POLL_SECONDS=1, so a step with a
 * zero-minute wait is dispatched within a second or two instead of on the
 * fifteen-second production cadence.
 */

async function stubCount(page: Page): Promise<number> {
  const res = await page.request.get(`${STUB_BASE}/count`);
  if (!res.ok()) throw new Error(`stub /count failed: ${res.status()}`);
  return ((await res.json()) as { count: number }).count;
}

test("escalation pages a second channel, and acknowledging stops the ladder", async ({
  page,
}) => {
  test.setTimeout(120_000);

  await loginViaUI(page);
  const token = await authToken(page);
  const channelName = uniqueName("Escalation sink");
  const policyName = uniqueName("Escalation ladder");
  const monitorName = uniqueName("Escalation target");

  // A webhook channel pointed at the counting stub. Created via the API: this
  // spec is about the escalation flow, and notification CRUD already has its
  // own browser coverage in 03-notification.spec.ts.
  const channelRes = await page.request.post(`${API_BASE}/api/notifications`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      name: channelName,
      type: "webhook",
      active: true,
      config: { url: `${STUB_BASE}/notify` },
    },
  });
  expect(channelRes.ok(), await channelRes.text()).toBeTruthy();

  // --- Policy CRUD through the real UI ------------------------------------
  await page.goto(`${BASE_URL}/escalation-policies`);
  await page.getByRole("button", { name: "New policy" }).click();
  const dialog = page.getByRole("dialog");
  await expect(dialog).toBeVisible();

  await dialog.locator("#esc-name").fill(policyName);

  // Step 1 fires immediately; step 2 would follow immediately after it. The
  // acknowledgement landing between them is what must stop step 2.
  const stepAt = (index: number) =>
    dialog.getByTestId("escalation-step").nth(index);

  await expect(stepAt(0)).toBeVisible({ timeout: 15_000 });
  await dialog.locator("#esc-wait-0").fill("0");
  await stepAt(0)
    .locator("label", { hasText: channelName })
    .locator("input[type=checkbox]")
    .check();

  await dialog.getByRole("button", { name: "Add step" }).click();
  await expect(stepAt(1)).toBeVisible();
  await dialog.locator("#esc-wait-1").fill("0");
  await stepAt(1)
    .locator("label", { hasText: channelName })
    .locator("input[type=checkbox]")
    .check();

  await dialog.getByRole("button", { name: "Save", exact: true }).click();
  await expect(dialog).toBeHidden({ timeout: 15_000 });

  const card = page
    .getByTestId("escalation-policy-card")
    .filter({ hasText: policyName })
    .first();
  await expect(card).toBeVisible({ timeout: 15_000 });
  // Two steps really landed — assert the effect, not the redirect.
  await expect(card).toContainText("2 steps");

  const policiesRes = await page.request.get(
    `${API_BASE}/api/escalation-policies`,
    {
      headers: { Authorization: `Bearer ${token}` },
    },
  );
  const policies = (await policiesRes.json()) as Array<{
    id: number;
    name: string;
  }>;
  const policyId = policies.find((p) => p.name === policyName)?.id;
  expect(
    policyId,
    "the policy created in the UI should be listed",
  ).toBeTruthy();

  // --- A monitor that is going to fail ------------------------------------
  // Port 9 (discard) refuses on the loopback, so the check fails immediately
  // and max_retries 0 means the very first failure is a confirmed DOWN.
  const monitorRes = await page.request.post(`${API_BASE}/api/monitors`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      name: monitorName,
      type: "tcp",
      active: true,
      interval: 20,
      timeout: 2,
      retry_interval: 20,
      max_retries: 0,
      resend_interval: 0,
      config: { hostname: "127.0.0.1", port: 9 },
    },
  });
  expect(monitorRes.ok(), await monitorRes.text()).toBeTruthy();
  const monitorId = ((await monitorRes.json()) as { id: number }).id;

  const assignRes = await page.request.put(
    `${API_BASE}/api/monitors/${monitorId}/escalation-policy`,
    {
      headers: { Authorization: `Bearer ${token}` },
      data: { policy_id: policyId },
    },
  );
  expect(assignRes.ok(), await assignRes.text()).toBeTruthy();

  // --- DOWN → the ladder starts -------------------------------------------
  // Poll the API rather than the page: the alerts list fetches once on mount
  // and does not live-refresh, so watching a stale render would just time out.
  await expect
    .poll(
      async () => {
        const res = await page.request.get(
          `${API_BASE}/api/alerts?open=1&monitor_id=${monitorId}`,
          {
            headers: { Authorization: `Bearer ${token}` },
          },
        );
        if (!res.ok()) return 0;
        return ((await res.json()) as unknown[]).length;
      },
      {
        timeout: 60_000,
        message: "a firing alert should open for the failing monitor",
      },
    )
    .toBe(1);

  // Step 1 (wait 0) is dispatched by the runner within a poll or two.
  await expect
    .poll(() => stubCount(page), {
      timeout: 30_000,
      message: "step 1 should reach the webhook channel",
    })
    .toBeGreaterThanOrEqual(1);

  // --- Acknowledge, in the browser ----------------------------------------
  await page.goto(`${BASE_URL}/alerts`);
  const liveRow = page
    .locator("div.px-5.py-4")
    .filter({ hasText: monitorName })
    .first();
  await expect(liveRow).toBeVisible({ timeout: 30_000 });
  await liveRow.getByRole("button", { name: "Acknowledge" }).click();
  await expect(liveRow.getByText("Acknowledged", { exact: false })).toBeVisible(
    {
      timeout: 15_000,
    },
  );

  // The badge must say the ladder stopped — and the alert must STAY OPEN.
  // Acked means "stop escalating", not "resolved".
  const badge = liveRow.getByTestId("alert-escalation-badge");
  await expect
    .poll(async () => badge.getAttribute("data-escalation-status"), {
      timeout: 20_000,
      message: "the escalation should be canceled by the acknowledgement",
    })
    .toBe("canceled");

  // --- No later step, proven by the count not moving -----------------------
  const afterAck = await stubCount(page);
  await page.waitForTimeout(8_000); // several runner polls
  expect(
    await stubCount(page),
    "no escalation step may fire after the acknowledgement",
  ).toBe(afterAck);
});
