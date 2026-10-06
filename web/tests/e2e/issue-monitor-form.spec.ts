/**
 * Browser regressions for the MonitorForm triad: GitHub #60 (atomic
 * remote-only create), #58 (edits preserve the pause state) and #59 (gRPC
 * canonical config keys).
 *
 * #60 is the load-bearing one: create must be exactly ONE POST whose body
 * carries the initial assignment set (`probe_ids` / `health_policy` /
 * `probe_bindings`) so the backend commits monitor + desired set atomically
 * (CreateMonitorRequest -> services.CreateWithAssignments). A rejected set
 * must leave no monitor behind. That rejection is exercised against the REAL
 * backend: the selected probe registration is deleted before submit, so the
 * atomic create fails with 409 "unknown probe registration" and nothing is
 * persisted.
 */
import { expect, test, type Locator, type Page } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
  type MonitorView,
} from "./helpers";

interface CreatedProbe {
  id: string;
  name: string;
}

/** Register a remote probe through the admin API (setup, not the UI under test). */
async function registerProbe(page: Page, token: string): Promise<CreatedProbe> {
  const name = uniqueName("Region SG");
  const response = await page.request.post(`${API_BASE}/api/probes`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      probe_id: crypto.randomUUID(),
      key: uniqueName("sg"),
      name,
      location: "Singapore",
      endpoint: "127.0.0.1:1",
      tls_fingerprint: "a".repeat(64),
    },
  });
  expect(response.ok(), await response.text()).toBe(true);
  const created = (await response.json()) as { id: string };
  return { id: created.id, name };
}

interface MonitorTraffic {
  creates: Record<string, unknown>[];
  edits: string[];
  deletes: string[];
  assignmentWrites: string[];
}

/**
 * Watch every /api/monitors write so "exactly one POST, no post-create
 * PUT/resume/delete compensation" is observable (issue #60).
 */
function trackMonitorWrites(page: Page): MonitorTraffic {
  const traffic: MonitorTraffic = {
    creates: [],
    edits: [],
    deletes: [],
    assignmentWrites: [],
  };
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (!pathname.startsWith("/api/monitors")) return;
    const method = request.method();
    if (method === "POST" && pathname === "/api/monitors") {
      traffic.creates.push(request.postDataJSON() as Record<string, unknown>);
    } else if (
      method === "PUT" &&
      /^\/api\/monitors\/[^/]+\/probes$/.test(pathname)
    ) {
      traffic.assignmentWrites.push(pathname);
    } else if (method === "PUT") {
      traffic.edits.push(pathname);
    } else if (method === "DELETE") {
      traffic.deletes.push(pathname);
    }
  });
  return traffic;
}

async function monitorsNamed(
  page: Page,
  token: string,
  name: string,
): Promise<MonitorView[]> {
  const response = await page.request.get(`${API_BASE}/api/monitors`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(response.ok()).toBeTruthy();
  return ((await response.json()) as MonitorView[]).filter(
    (candidate) => candidate.name === name,
  );
}

/** Open the create dialog with the shared fields filled (HTTP monitor). */
async function openCreateForm(
  page: Page,
  monitorName: string,
): Promise<Locator> {
  await page.goto(`${BASE_URL}/monitors`);
  await page.getByRole("button", { name: "Add Monitor" }).click();
  const dialog = page.getByRole("dialog", { name: "Create Monitor" });
  await expect(dialog).toBeVisible();
  await dialog.locator("#monitor-name").fill(monitorName);
  await dialog.locator("#cfg-url").fill(`${BASE_URL}/api/health/live`);
  return dialog;
}

function regionsIn(dialog: Locator): Locator {
  return dialog.getByRole("region", { name: "Check regions", exact: true });
}

/** Pick one remote probe and deselect Local (the remote-only draft). */
async function selectRemoteOnly(
  regions: Locator,
  probeName: string,
): Promise<void> {
  const local = regions.getByRole("checkbox", { name: /^Local/ });
  await expect(local).toBeVisible({ timeout: 15_000 });
  await local.uncheck();
  await regions.getByRole("checkbox", { name: new RegExp(probeName) }).check();
}

test("a remote-only create is one POST carrying the full initial assignment set (issue #60)", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const probe = await registerProbe(page, token);
  const traffic = trackMonitorWrites(page);
  const monitorName = uniqueName("Remote-only");
  const dialog = await openCreateForm(page, monitorName);
  const regions = regionsIn(dialog);
  await selectRemoteOnly(regions, probe.name);
  await regions.getByLabel("Overall health policy").selectOption("all_down");

  const createdResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/monitors") &&
      response.request().method() === "POST",
  );
  await dialog.getByRole("button", { name: "Create Monitor" }).click();
  expect((await createdResponse).status()).toBe(201);
  await expect(dialog).not.toBeVisible({ timeout: 15_000 });

  // Exactly one POST, and it carried the draft — chosen probes and policy,
  // never local. No post-create PUT/resume/delete compensation exists.
  expect(traffic.creates).toHaveLength(1);
  expect(traffic.creates[0].probe_ids).toEqual([probe.id]);
  expect(traffic.creates[0].health_policy).toBe("all_down");
  expect(traffic.creates[0].probe_bindings).toBeUndefined();
  expect(traffic.edits).toEqual([]);
  expect(traffic.deletes).toEqual([]);
  expect(traffic.assignmentWrites).toEqual([]);

  // The persisted desired set is the draft: remote-only, no Local assignment.
  const matches = await monitorsNamed(page, token, monitorName);
  expect(matches).toHaveLength(1);
  const assignments = await page.request.get(
    `${API_BASE}/api/monitors/${matches[0].id}/probes`,
    { headers: { Authorization: `Bearer ${token}` } },
  );
  expect(assignments.ok(), await assignments.text()).toBe(true);
  const set = (await assignments.json()) as {
    health_policy: string;
    assignments: { probe_id: string }[];
  };
  expect(set.assignments.map((row) => row.probe_id)).toEqual([probe.id]);
  expect(set.health_policy).toBe("all_down");
});

test("a rejected initial set leaves no monitor behind and the retry creates exactly one (issue #60)", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  const stale = await registerProbe(page, token);
  const fresh = await registerProbe(page, token);
  const traffic = trackMonitorWrites(page);
  const monitorName = uniqueName("Rejected then retried");
  const dialog = await openCreateForm(page, monitorName);
  const regions = regionsIn(dialog);
  await selectRemoteOnly(regions, stale.name);

  // The selected registration disappears before submit: the REAL backend
  // rejects the atomic create (409 unknown probe registration) and must
  // leave no active local-default monitor behind.
  const removed = await page.request.delete(
    `${API_BASE}/api/probes/${stale.id}`,
    { headers },
  );
  expect(removed.ok(), await removed.text()).toBe(true);

  const rejectedResponse = page.waitForResponse(
    (response) =>
      response.url().endsWith("/api/monitors") &&
      response.request().method() === "POST",
  );
  await dialog.getByRole("button", { name: "Create Monitor" }).click();
  expect((await rejectedResponse).status()).toBe(409);
  await expect(dialog).toBeVisible();
  expect(await monitorsNamed(page, token, monitorName)).toEqual([]);

  // Retrying with a live selection creates exactly one monitor — no
  // duplicates and no stale half-created state.
  await regions
    .getByRole("checkbox", { name: new RegExp(stale.name) })
    .uncheck();
  await regions.getByRole("checkbox", { name: new RegExp(fresh.name) }).check();
  await dialog.getByRole("button", { name: "Create Monitor" }).click();
  await expect(dialog).not.toBeVisible({ timeout: 15_000 });

  expect(traffic.creates).toHaveLength(2);
  expect(traffic.creates[0].probe_ids).toEqual([stale.id]);
  expect(traffic.creates[1].probe_ids).toEqual([fresh.id]);
  expect(traffic.edits).toEqual([]);
  expect(traffic.deletes).toEqual([]);
  expect(traffic.assignmentWrites).toEqual([]);

  const matches = await monitorsNamed(page, token, monitorName);
  expect(matches).toHaveLength(1);
  const assignments = await page.request.get(
    `${API_BASE}/api/monitors/${matches[0].id}/probes`,
    { headers },
  );
  const set = (await assignments.json()) as {
    assignments: { probe_id: string }[];
  };
  expect(set.assignments.map((row) => row.probe_id)).toEqual([fresh.id]);
});

test("saving an edit on a paused monitor keeps it paused (issue #58)", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  const monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("Paused edit"),
  );
  const paused = await page.request.put(
    `${API_BASE}/api/monitors/${monitor.id}`,
    { headers, data: { active: false } },
  );
  expect(paused.ok(), await paused.text()).toBe(true);

  await page.goto(`${BASE_URL}/monitors`);
  const row = page.locator("table tr").filter({ hasText: monitor.name });
  await row.getByRole("button", { name: "Edit", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "Edit Monitor" });
  await expect(dialog).toBeVisible();
  const renamed = `${monitor.name} renamed`;
  await dialog.locator("#monitor-name").fill(renamed);
  await dialog.getByRole("button", { name: "Save Changes" }).click();
  await expect(dialog).not.toBeVisible({ timeout: 15_000 });

  const after = await page.request.get(
    `${API_BASE}/api/monitors/${monitor.id}`,
    { headers },
  );
  const updated = (await after.json()) as MonitorView & { active: boolean };
  expect(updated.name).toBe(renamed);
  // The update body omits `active`, so the edit never resumes the monitor.
  expect(updated.active).toBe(false);
});

test("gRPC monitors roundtrip the checker's canonical config keys (issue #59)", async ({
  page,
}) => {
  await loginViaUI(page);
  const token = await authToken(page);
  const headers = { Authorization: `Bearer ${token}` };
  const monitorName = uniqueName("gRPC health");
  await page.goto(`${BASE_URL}/monitors`);
  await page.getByRole("button", { name: "Add Monitor" }).click();
  const dialog = page.getByRole("dialog", { name: "Create Monitor" });
  await expect(dialog).toBeVisible();
  await dialog.locator("#monitor-name").fill(monitorName);
  await dialog.locator("#monitor-type").click();
  await page.getByRole("option", { name: "gRPC Health" }).click();
  await dialog.locator("#cfg-url").fill("127.0.0.1:50051");
  await dialog.locator("#cfg-service_name").fill("package.Health");
  await dialog.locator("#cfg-tls").check();
  await dialog.locator("#cfg-timeout").fill("7");
  await dialog.getByRole("button", { name: "Create Monitor" }).click();
  await expect(dialog).not.toBeVisible({ timeout: 15_000 });

  const matches = await monitorsNamed(page, token, monitorName);
  expect(matches).toHaveLength(1);
  // The checker's canonical keys roundtrip under their exact wire names
  // (issue #59). Note: the form also seeds the http type's default keys into
  // one shared config state (method, follow_redirects, ...) — pre-existing
  // cross-type noise the gRPC checker ignores, so only the canonical keys are
  // asserted here.
  expect(matches[0].config).toMatchObject({
    url: "127.0.0.1:50051",
    service_name: "package.Health",
    tls: true,
    timeout: 7,
  });
  expect(matches[0].config).not.toHaveProperty("hostname");
  expect(matches[0].config).not.toHaveProperty("service");

  // Reopen: the form seeds the canonical keys (url/service_name/tls/timeout),
  // and saving again roundtrips them unchanged.
  const row = page.locator("table tr").filter({ hasText: monitorName });
  await row.getByRole("button", { name: "Edit", exact: true }).click();
  const edit = page.getByRole("dialog", { name: "Edit Monitor" });
  await expect(edit.locator("#cfg-url")).toHaveValue("127.0.0.1:50051");
  await expect(edit.locator("#cfg-service_name")).toHaveValue("package.Health");
  await expect(edit.locator("#cfg-tls")).toBeChecked();
  await expect(edit.locator("#cfg-timeout")).toHaveValue("7");
  await edit.getByRole("button", { name: "Save Changes" }).click();
  await expect(edit).not.toBeVisible({ timeout: 15_000 });

  const after = await page.request.get(
    `${API_BASE}/api/monitors/${matches[0].id}`,
    { headers },
  );
  expect(((await after.json()) as MonitorView).config).toMatchObject({
    url: "127.0.0.1:50051",
    service_name: "package.Health",
    tls: true,
    timeout: 7,
  });
});
