import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
} from "./helpers";

test("admin registers, pauses and assigns a probe; stale edits and unknown coverage remain honest", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  await page.goto(`${BASE_URL}/probes/local`);
  await expect(
    page.getByRole("heading", { name: "Local", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Save changes", exact: true }),
  ).toHaveCount(0);
  await expect(
    page.getByRole("button", { name: "Delete registration", exact: true }),
  ).toHaveCount(0);
  const initializedProbeId = crypto.randomUUID();
  const initializedStreamId = crypto.randomUUID();
  const probeName = uniqueName("Singapore");
  await page.getByRole("link", { name: "Probes", exact: true }).click();
  await page
    .getByRole("button", { name: "Register probe", exact: true })
    .click();
  await page.getByLabel("Probe ID", { exact: true }).fill(initializedProbeId);
  await page.getByLabel("Key", { exact: true }).fill(uniqueName("sg"));
  await page.getByLabel("Name", { exact: true }).fill(probeName);
  await page.getByLabel("Location", { exact: true }).fill("Singapore");
  await page.getByLabel("Agent endpoint", { exact: true }).fill("127.0.0.1:1");
  await page
    .getByLabel("TLS SHA-256 fingerprint", { exact: true })
    .fill("a".repeat(64));
  await page
    .locator("form")
    .filter({ has: page.locator("#probe-registration-id") })
    .screenshot({ path: "/tmp/m5-registration-desktop.png" });
  await page
    .getByRole("button", { name: "Register probe", exact: true })
    .last()
    .click();
  await expect(
    page.getByRole("heading", { name: probeName, exact: true }),
  ).toBeVisible();
  const probeId = page.url().split("/").pop()!;
  expect(probeId).toBe(initializedProbeId);
  await expect(
    page.getByText(`Probe ID: ${initializedProbeId}`, { exact: true }),
  ).toBeVisible();
  await page.screenshot({ path: "/tmp/m5-probe-desktop.png", fullPage: true });
  await expect(
    page.getByText("Unreported", { exact: true }).first(),
  ).toBeVisible();
  await page.getByRole("button", { name: "Pause probe", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Resume probe", exact: true }),
  ).toBeVisible();
  const paused = await page.request.get(`${API_BASE}/api/probes/${probeId}`, {
    headers,
  });
  expect((await paused.json()).enabled).toBe(false);
  await page.getByRole("button", { name: "Resume probe", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Pause probe", exact: true }),
  ).toBeVisible();

  // The real unreachable agent returns a durable failure, never invented success.
  await page
    .getByLabel("Enrollment token", { exact: true })
    .fill(`phx_probe_enroll_${"x".repeat(48)}`);
  await expect(
    page.getByRole("button", { name: "Enroll agent", exact: true }),
  ).toBeDisabled();
  await page.locator("#probe-enrollment-stream").fill(initializedStreamId);
  await expect(
    page.getByRole("button", { name: "Enroll agent", exact: true }),
  ).toBeEnabled();
  await page
    .locator("form")
    .filter({ has: page.locator("#probe-enrollment-stream") })
    .screenshot({ path: "/tmp/m5-enrollment-enabled-desktop.png" });
  const enrollmentRequest = page.waitForRequest(
    (request) =>
      request.method() === "POST" &&
      request.url().endsWith(`/api/probes/${probeId}/enroll`),
  );
  await page.getByRole("button", { name: "Enroll agent", exact: true }).click();
  expect((await enrollmentRequest).postDataJSON().stream_id).toBe(
    initializedStreamId,
  );
  await expect(
    page.getByLabel("Enrollment token", { exact: true }),
  ).toHaveValue("");
  await expect(
    page.getByRole("region", { name: "Operation progress" }),
  ).toContainText("Failed");
  await page
    .locator("form")
    .filter({ has: page.locator("#probe-enrollment-stream") })
    .screenshot({ path: "/tmp/m5-enrollment-desktop.png" });

  const monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("fleet-assignment"),
  );
  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
  await page.locator("summary").filter({ hasText: "Check regions" }).click();
  const editor = page.getByRole("region", {
    name: "Check regions",
    exact: true,
  });
  await editor.getByRole("checkbox", { name: new RegExp(probeName) }).check();
  await editor
    .getByRole("button", { name: "Save changes", exact: true })
    .click();
  await expect(editor.getByRole("status")).toContainText(
    "Desired regions saved",
  );
  const assignments = await page.request.get(
    `${API_BASE}/api/monitors/${monitor.id}/probes`,
    { headers },
  );
  const assigned = await assignments.json();
  expect(
    assigned.assignments.map((a: { probe_id: string }) => a.probe_id),
  ).toContain(probeId);
  await page
    .getByTestId("regional-health")
    .getByRole("button", { name: "Refresh", exact: true })
    .click();
  const region = page
    .getByTestId("regional-health-row")
    .filter({ hasText: probeName });
  await expect(region).toContainText("Unknown");
  await expect(region).toContainText("No observations yet");
  await page.getByTestId("regional-health").screenshot({
    path: "/tmp/m5-regional-health-desktop.png",
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(() =>
      page
        .locator("aside")
        .evaluate((sidebar) => sidebar.getBoundingClientRect().right),
    )
    .toBeLessThanOrEqual(0);
  await page.getByTestId("regional-health").screenshot({
    path: "/tmp/m5-regional-health-mobile.png",
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page.setViewportSize({ width: 1280, height: 720 });

  // A concurrent policy write makes this editor stale; the UI must keep its edits.
  const changed = await page.request.put(
    `${API_BASE}/api/monitors/${monitor.id}/probes`,
    {
      headers,
      data: {
        expected_revision: assigned.revision,
        probe_ids: ["local", probeId],
        health_policy: "all_down",
        alert_delivery: "regional",
      },
    },
  );
  expect(changed.ok()).toBe(true);
  await editor.getByRole("checkbox", { name: new RegExp(probeName) }).uncheck();
  await editor
    .getByRole("button", { name: "Save changes", exact: true })
    .click();
  await expect(editor.getByRole("alert")).toContainText("changed elsewhere");
  const retained = await page.request.get(
    `${API_BASE}/api/monitors/${monitor.id}/probes`,
    { headers },
  );
  const current = await retained.json();
  expect(current.assignments).toHaveLength(2);

  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(`${BASE_URL}/probes/${probeId}`);
  await expect(
    page.getByRole("heading", { name: probeName, exact: true }),
  ).toBeVisible();
  await page.screenshot({ path: "/tmp/m5-probe-mobile.png", fullPage: true });
  await page
    .locator("form")
    .filter({ has: page.locator("#probe-enrollment-stream") })
    .screenshot({ path: "/tmp/m5-enrollment-mobile.png" });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth,
    ),
  ).toBe(true);
  await page
    .getByRole("button", { name: "Delete registration", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Confirm action", exact: true })
    .click();
  await expect(page.getByRole("alert")).toBeVisible();
  expect(
    (
      await page.request.get(`${API_BASE}/api/probes/${probeId}`, { headers })
    ).ok(),
  ).toBe(true);

  const unassigned = await page.request.put(
    `${API_BASE}/api/monitors/${monitor.id}/probes`,
    {
      headers,
      data: {
        expected_revision: current.revision,
        probe_ids: ["local"],
        health_policy: "any_down",
        alert_delivery: "regional",
      },
    },
  );
  expect(unassigned.ok()).toBe(true);
  await page.getByRole("button", { name: "Cancel", exact: true }).click();
  await page.getByRole("button", { name: "Revoke probe", exact: true }).click();
  await page.getByLabel("Reason", { exact: true }).fill("Retiring test region");
  await page
    .getByRole("button", { name: "Confirm action", exact: true })
    .click();
  await expect(
    page.getByText(
      "Hub access revoked. Remote execution has not been confirmed stopped.",
      { exact: true },
    ),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Delete registration", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Confirm action", exact: true })
    .click();
  await expect(page).toHaveURL(`${BASE_URL}/probes`);
  expect(
    (
      await page.request.get(`${API_BASE}/api/probes/${probeId}`, { headers })
    ).status(),
  ).toBe(404);
});

test("a scoped reader cannot discover the fleet from navigation or a direct route", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const username = uniqueName("regional-reader");
  const created = await page.request.post(`${API_BASE}/api/users`, {
    headers: { Authorization: `Bearer ${token}` },
    data: { username, password: "ReaderPassword123!", is_admin: false },
  });
  expect(created.ok(), await created.text()).toBe(true);
  await page.evaluate(() => localStorage.removeItem("phoenix_jwt"));
  await loginViaUI(page, username, "ReaderPassword123!");
  await expect(
    page.getByRole("link", { name: "Probes", exact: true }),
  ).toHaveCount(0);
  const requests: string[] = [];
  page.on("request", (request) => {
    if (request.url().includes("/api/probes")) requests.push(request.url());
  });
  await page.goto(`${BASE_URL}/probes`);
  await expect(
    page.getByText("Probe management requires an administrator."),
  ).toBeVisible();
  expect(requests).toEqual([]);
});

test("regional acknowledgement stays pending until its durable receipt confirms application", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("regional-ack"),
  );
  const sourceId = crypto.randomUUID();
  const commandId = crypto.randomUUID();
  let confirmed = false;
  let issued = 0;
  const prefix = `**/api/monitors/${monitor.id}/probe-alerts`;
  const incident = {
    source_alert_id: sourceId,
    monitor_id: monitor.id,
    probe_id: crypto.randomUUID(),
    probe_name: "Singapore",
    location: "SG",
    assignment_generation: "9007199254740993",
    status: "firing",
    subject_kind: "availability",
    reason: "Target unreachable",
    started_at: new Date().toISOString(),
    acked_at: null as string | null,
    resolved_at: null,
  };
  await page.route(prefix, (route) =>
    route.fulfill({
      json: [
        {
          ...incident,
          status: confirmed ? "acked" : "firing",
          acked_at: confirmed ? new Date().toISOString() : null,
        },
      ],
    }),
  );
  await page.route(`${prefix}/${sourceId}/ack`, (route) => {
    issued += 1;
    expect(route.request().postDataJSON().command_id).toMatch(
      /^[0-9a-f-]{36}$/,
    );
    return route.fulfill({
      status: 202,
      json: {
        command_id: commandId,
        status: "pending",
        remote_confirmed: false,
      },
    });
  });
  await page.route(`${prefix}/${sourceId}/ack/${commandId}`, (route) =>
    route.fulfill({
      json: {
        command_id: commandId,
        status: confirmed ? "applied" : "pending",
        remote_confirmed: confirmed,
      },
    }),
  );
  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
  const incidents = page.getByRole("region", {
    name: "Regional incidents",
    exact: true,
  });
  await expect(incidents).toContainText("Region: Singapore");
  await incidents
    .getByRole("button", { name: "Acknowledge", exact: true })
    .click();
  await expect(incidents).toContainText("Waiting for the region to confirm");
  await expect(
    incidents.getByText("Alert acknowledged", { exact: true }),
  ).toHaveCount(0);
  await expect(
    incidents.getByRole("button", { name: "Acknowledge", exact: true }),
  ).toHaveCount(0);
  expect(issued).toBe(1);
  confirmed = true;
  await incidents.getByRole("button", { name: "Refresh", exact: true }).click();
  await expect(
    incidents.getByText("Alert acknowledged", { exact: true }),
  ).toBeVisible();
  await expect(
    incidents.getByText("Acknowledged", { exact: true }),
  ).toBeVisible();
});

test("atomic monitor creation preserves selected regions across a rejected create and retry", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  const probeName = uniqueName("create-region");
  const registration = await page.request.post(`${API_BASE}/api/probes`, {
    headers,
    data: {
      probe_id: crypto.randomUUID(),
      key: uniqueName("create"),
      name: probeName,
      location: "SG",
      endpoint: "127.0.0.1:1",
      tls_fingerprint: "b".repeat(64),
    },
  });
  expect(registration.ok()).toBe(true);
  const probe = await registration.json();
  const monitorName = uniqueName("create-regional");
  let createAttempts = 0;
  // Fault injection is confined to the atomic create request. The second
  // attempt reaches the real backend and persists the selected set together
  // with the monitor; there is no later assignment PUT to retry.
  await page.route("**/api/monitors", (route) => {
    if (route.request().method() === "POST") {
      createAttempts++;
      expect(route.request().postDataJSON().probe_ids).toEqual(
        expect.arrayContaining(["local", probe.id]),
      );
      if (createAttempts === 1) {
        return route.fulfill({
          status: 503,
          json: {
            code: "assignment_unavailable",
            error: "Assignment temporarily unavailable",
          },
        });
      }
    }
    return route.continue();
  });
  await page.goto(`${BASE_URL}/monitors`);
  await page.getByRole("button", { name: "Add Monitor", exact: true }).click();
  const dialog = page.getByRole("dialog", {
    name: "Create Monitor",
    exact: true,
  });
  await dialog.locator("#monitor-name").fill(monitorName);
  await dialog.locator("#cfg-url").fill(`${BASE_URL}/api/health/live`);
  const regions = dialog.getByRole("region", {
    name: "Check regions",
    exact: true,
  });
  await regions.getByRole("checkbox", { name: new RegExp(probeName) }).check();
  await dialog
    .getByRole("button", { name: "Create Monitor", exact: true })
    .click();
  await expect(
    page.getByText("Assignment temporarily unavailable", { exact: true }),
  ).toBeVisible();
  await expect(dialog).toBeVisible();
  const rejectedList = await page.request.get(`${API_BASE}/api/monitors`, {
    headers,
  });
  expect(
    ((await rejectedList.json()) as Array<{ name: string }>).filter(
      (monitor) => monitor.name === monitorName,
    ),
  ).toHaveLength(0);
  await expect(
    regions.getByRole("checkbox", { name: new RegExp(probeName) }),
  ).toBeChecked();
  await dialog
    .getByRole("button", { name: "Create Monitor", exact: true })
    .click();
  await expect(dialog).toBeHidden();
  const list = await page.request.get(`${API_BASE}/api/monitors`, { headers });
  const monitors = (
    (await list.json()) as Array<{ id: number; name: string }>
  ).filter((monitor) => monitor.name === monitorName);
  expect(monitors).toHaveLength(1);
  expect(createAttempts).toBe(2);
  const response = await page.request.get(
    `${API_BASE}/api/monitors/${monitors[0].id}/probes`,
    { headers },
  );
  const assigned = await response.json();
  expect(
    assigned.assignments.map(
      (assignment: { probe_id: string }) => assignment.probe_id,
    ),
  ).toEqual(expect.arrayContaining(["local", probe.id]));
});
