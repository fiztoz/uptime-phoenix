import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
} from "./helpers.js";

// Registration identity: the operator-supplied key is a human label; the
// canonical probe id is assigned by the hub and returned in the 201 view.
const PROBE_KEY = "vm-sg";

/**
 * M5 regional history/latency-selection UI.
 *
 * Asserts the section-7.2 compatibility behavior in the browser: a multi-probe
 * monitor serves the overall timeline (latency unavailable, UNKNOWN visible)
 * and the region picker switches latency to the relationship-checked regional
 * endpoint.
 */
test("multi-probe monitor shows overall latency-unavailable state and regional latency selection", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("regional-ui"),
  );

  const probeRes = await page.request.post(`${API_BASE}/api/probes`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      key: PROBE_KEY,
      name: "Singapore",
      location: "SG",
      endpoint: "sg.example:8443",
      tls_fingerprint: "a".repeat(64),
    },
  });
  expect(probeRes.ok(), await probeRes.text()).toBe(true);
  const probe = (await probeRes.json()) as { id: string };
  expect(probe.id).toBeTruthy();

  const putRes = await page.request.put(
    `${API_BASE}/api/monitors/${monitor.id}/probes`,
    {
      headers: { Authorization: `Bearer ${token}` },
      data: {
        expected_revision: "1",
        probe_ids: ["local", probe.id],
        health_policy: "any_down",
        alert_delivery: "regional",
      },
    },
  );
  expect(putRes.ok(), await putRes.text()).toBe(true);

  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);

  // Overall stream: no measured latency, interval bands and the hint stay visible.
  await expect(page.getByTestId("latency-unavailable-hint")).toBeVisible({
    timeout: 15_000,
  });
  await expect(page.getByTestId("overall-intervals")).toBeVisible();

  // Latency selection: the region picker offers overall plus each member.
  const picker = page.getByLabel("Region");
  await expect(picker).toBeVisible();
  await picker.click();
  await expect(page.getByRole("option", { name: "All regions (overall)" })).toBeVisible();
  await page.keyboard.press("Escape");

  // Selecting the remote region switches latency to the regional endpoint: the
  // overall hint must clear even though the region has no samples yet.
  await picker.click();
  await page.getByRole("option", { name: "Singapore" }).click();
  await expect(page.getByTestId("latency-unavailable-hint")).toBeHidden({
    timeout: 15_000,
  });

  // Switching back restores the overall compatibility state.
  await picker.click();
  await page.getByRole("option", { name: "All regions (overall)" }).click();
  await expect(page.getByTestId("latency-unavailable-hint")).toBeVisible({
    timeout: 15_000,
  });
});
