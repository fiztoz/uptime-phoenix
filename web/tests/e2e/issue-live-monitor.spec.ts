/**
 * Browser regressions for the live monitor / probe surfaces: GitHub #57
 * (overall_status on the monitor detail page), #61 (probe.status payload
 * identity) and #62 (rolling response-chart window).
 *
 * REAL (not mocked): the Go app under test (the Playwright webServer builds
 * and runs the real binary), login + session, monitor/probe creation through
 * the admin API, and every REST GET the pages issue unless a test deliberately
 * replaces that specific response.
 *
 * MOCKED (explicitly labelled per test):
 *  - WS frame stream: `page.routeWebSocket` WITHOUT `connectToServer()` — the
 *    hub's frames are replaced by deterministic injections, so the real
 *    scheduler can never interleave a real heartbeat with the scripted ones.
 *  - REST history/chart endpoints whose payload shape is under test, served
 *    from mutable fixtures so "a refreshed payload arrived" is observable.
 */
import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
  type MonitorView,
} from "./helpers";

/** Mocked WS: the test owns every frame the page receives. */
interface LiveSocketMock {
  send(type: string, payload: unknown): void;
}

function mockLiveSocket(page: Page, snapshot: () => unknown[]): LiveSocketMock {
  let socket: WebSocketRoute | undefined;
  void page.routeWebSocket(/\/ws(?:\?|$)/, (ws) => {
    // Deliberately no connectToServer(): fully mocked frame stream.
    socket = ws;
    ws.send(JSON.stringify({ type: "monitor.list", payload: snapshot() }));
  });
  return {
    send(type, payload) {
      expect(socket, "WS mock should have a connected client").toBeDefined();
      socket?.send(JSON.stringify({ type, payload }));
    },
  };
}

/** Row shape for a mocked `monitor.list` snapshot entry. */
function liveMonitorRow(monitor: MonitorView, status: string) {
  return {
    id: monitor.id,
    name: monitor.name,
    type: monitor.type,
    status,
    active: true,
    config: monitor.config ?? {},
    created_at: new Date().toISOString(),
    updated_at: new Date().toISOString(),
  };
}

interface HistoryRow {
  id: number;
  monitor_id: number;
  status: string;
  ping: number;
  message: string;
  time: string;
  important: boolean;
}

function historyRow(
  monitorId: number,
  status: string,
  time: string,
  message: string,
  id: number,
): HistoryRow {
  return {
    id,
    monitor_id: monitorId,
    status,
    ping: 12,
    message,
    time,
    important: true,
  };
}

test("overall DOWN survives local UP heartbeats and stale history rows (#57)", async ({
  page,
}) => {
  // --- MOCK: WS frames + the two history endpoints (payload under test). ---
  let monitor: MonitorView | null = null;
  const live = mockLiveSocket(page, () =>
    monitor ? [liveMonitorRow(monitor, "down")] : [],
  );

  const staleTime = new Date(Date.now() - 60 * 60 * 1000).toISOString();
  await page.route(
    /\/api\/monitors\/\d+\/heartbeats(\?|$)/,
    (route) =>
      void route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(
          new URL(route.request().url()).searchParams.get("important") ===
            "true"
            ? // Stale history row: older than the live stream, and UP.
              [historyRow(monitor!.id, "up", staleTime, "stale history row", 1)]
            : [],
        ),
      }),
  );
  await page.route(
    /\/api\/monitors\/\d+\/heartbeats\/chart(\?|$)/,
    (route) =>
      void route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          buckets: [],
          downtime_intervals: [],
          unknown_intervals: [],
          latency_available: true,
        }),
      }),
  );

  // --- REAL: login + monitor creation through the admin API. ---
  await loginViaUI(page);
  const token = await authToken(page);
  monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("overall-down"),
  );

  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);

  // Wait for the stale history row to be applied first — the badge assertion
  // below must run against the post-load state, not the first-paint one.
  await expect(page.getByText("stale history row")).toBeVisible({
    timeout: 15_000,
  });

  // The live WS row reports overall DOWN while the (older) history row says
  // UP: the badge must follow live health, not the stale row (#57).
  await expect(page.getByTestId("monitor-status-pill")).toContainText("Down");

  // MOCK (WS): a local measurement reports UP but the policy result is DOWN
  // (any_down over local + remote). The local UP must never surface as the
  // overall health of the monitor.
  live.send("heartbeat", {
    monitor_id: monitor.id,
    status: "up",
    overall_status: "down",
    time: new Date(Date.now() + 1000).toISOString(),
    ping: 42,
    msg: "local check passed",
    projection_version: 1,
  });

  // Overall timeline row carries the policy status (DOWN), not the local UP.
  await expect(
    page
      .getByTestId("recent-check-timeline")
      .getByRole("button", { name: /^DOWN, / }),
  ).toBeVisible({ timeout: 10_000 });

  // Status history gains a DOWN transition row for the live check.
  const transitionRow = page
    .getByRole("row")
    .filter({ hasText: "local check passed" });
  await expect(transitionRow).toBeVisible({ timeout: 10_000 });
  await expect(transitionRow).toContainText("Down");

  // Stability: neither a later history hint nor the local UP re-flips the
  // badge back to Operational.
  await page.waitForTimeout(1200);
  await expect(page.getByTestId("monitor-status-pill")).toContainText("Down");
});

test("monitor.health refetches the overall history stream (#57)", async ({
  page,
}) => {
  // --- MOCK: WS frames + history endpoint with a mutable fixture. ---
  let monitor: MonitorView | null = null;
  const live = mockLiveSocket(page, () =>
    monitor ? [liveMonitorRow(monitor, "up")] : [],
  );

  let importantCalls = 0;
  let historyFixture: HistoryRow[] = [];
  await page.route(/\/api\/monitors\/\d+\/heartbeats(\?|$)/, (route) => {
    const important =
      new URL(route.request().url()).searchParams.get("important") === "true";
    if (important) importantCalls += 1;
    void route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(important ? historyFixture : []),
    });
  });
  await page.route(
    /\/api\/monitors\/\d+\/heartbeats\/chart(\?|$)/,
    (route) =>
      void route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          buckets: [],
          downtime_intervals: [],
          unknown_intervals: [],
          latency_available: true,
        }),
      }),
  );

  // --- REAL: login + monitor creation. ---
  await loginViaUI(page);
  const token = await authToken(page);
  monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("health-refresh"),
  );

  const seedTime = new Date(Date.now() - 60 * 60 * 1000).toISOString();
  historyFixture = [
    historyRow(monitor.id, "up", seedTime, "seed history row", 1),
  ];

  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
  await expect(page.getByText("seed history row")).toBeVisible({
    timeout: 15_000,
  });
  expect(importantCalls).toBe(1);

  // MOCK (WS): an overall health transition rewrites the synthesized overall
  // stream server-side; the page must refetch it instead of keeping the rows
  // fetched at mount (#57).
  historyFixture = [
    historyRow(
      monitor.id,
      "down",
      new Date().toISOString(),
      "refreshed overall row",
      2,
    ),
  ];
  live.send("monitor.health", {
    monitor_id: monitor.id,
    projection_version: "1",
    as_of: new Date().toISOString(),
    status: "down",
  });

  await expect(page.getByText("refreshed overall row")).toBeVisible({
    timeout: 10_000,
  });
  // The refetch is the load-bearing effect: a fresh GET for the overall
  // history fired after the event (debounced ~2s), not a cache hit.
  await expect
    .poll(() => importantCalls, { timeout: 5_000 })
    .toBeGreaterThanOrEqual(2);
});

test("probe.status {id} refetches the matching probe detail and fleet list (#61)", async ({
  page,
}) => {
  // --- MOCK: WS frames only; all probe REST reads stay REAL. ---
  const live = mockLiveSocket(page, () => []);

  // --- REAL: login + probe registration through the admin API. ---
  await loginViaUI(page);
  const token = await authToken(page);
  const probeName = uniqueName("Status event probe");
  const probeRes = await page.request.post(`${API_BASE}/api/probes`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      key: uniqueName("status-event"),
      name: probeName,
      location: "SG",
      endpoint: "sg.example:8443",
      tls_fingerprint: "a".repeat(64),
    },
  });
  expect(probeRes.ok(), await probeRes.text()).toBe(true);
  const probe = (await probeRes.json()) as { id: string };
  expect(probe.id).toBeTruthy();

  let detailCalls = 0;
  let listCalls = 0;
  page.on("request", (request) => {
    const { pathname } = new URL(request.url());
    if (request.method() !== "GET") return;
    if (pathname === `/api/probes/${probe.id}`) detailCalls += 1;
    if (pathname === "/api/probes") listCalls += 1;
  });

  // ---- Detail page ----
  await page.goto(`${BASE_URL}/probes/${probe.id}`);
  await expect(page.getByRole("heading", { name: probeName })).toBeVisible({
    timeout: 15_000,
  });
  await page.waitForTimeout(600); // settle the initial detail load
  const detailBaseline = detailCalls;
  expect(detailBaseline).toBeGreaterThanOrEqual(1);

  // MOCK (WS): `probe.status` names the probe with `id` — the detail page
  // must refetch on it (#61: a shared probe_id-only check dropped these).
  live.send("probe.status", { id: probe.id, status: "disconnected" });
  await expect
    .poll(() => detailCalls, { timeout: 5_000 })
    .toBe(detailBaseline + 1);

  // MOCK (WS): a status event for a DIFFERENT probe must not refetch ours.
  live.send("probe.status", { id: "not-our-probe", status: "online" });
  await page.waitForTimeout(1200);
  expect(detailCalls).toBe(detailBaseline + 1);

  // MOCK (WS): `probe.config.status` identifies with `probe_id` — the other
  // half of the identifier contract.
  live.send("probe.config.status", { probe_id: probe.id, status: "applied" });
  await expect
    .poll(() => detailCalls, { timeout: 5_000 })
    .toBe(detailBaseline + 2);

  // ---- Fleet list page ----
  await page.goto(`${BASE_URL}/probes`);
  await expect(
    page
      .getByText(probeName, { exact: true })
      .filter({ visible: true })
      .first(),
  ).toBeVisible({ timeout: 15_000 });
  await page.waitForTimeout(600); // settle the initial list load
  const listBaseline = listCalls;
  expect(listBaseline).toBeGreaterThanOrEqual(1);

  // MOCK (WS): the same {id} identity on the list page.
  live.send("probe.status", { id: probe.id, status: "online" });
  await expect
    .poll(() => listCalls, { timeout: 5_000 })
    .toBeGreaterThan(listBaseline);
});

test("response chart window end advances with an accepted refreshed payload (#62)", async ({
  page,
}) => {
  // --- MOCK: WS frames + chart endpoint with a mutable fixture. ---
  let monitor: MonitorView | null = null;
  const live = mockLiveSocket(page, () =>
    monitor ? [liveMonitorRow(monitor, "up")] : [],
  );

  let chartCalls = 0;
  let chartFixture: unknown = null;
  await page.route(/\/api\/monitors\/\d+\/heartbeats\/chart(\?|$)/, (route) => {
    chartCalls += 1;
    void route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(chartFixture),
    });
  });
  await page.route(
    /\/api\/monitors\/\d+\/heartbeats(\?|$)/,
    (route) =>
      void route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify([]),
      }),
  );

  // --- REAL: login + monitor creation. ---
  await loginViaUI(page);
  const token = await authToken(page);
  monitor = await createHttpMonitorViaApi(
    page,
    token,
    uniqueName("chart-window"),
  );

  const now = Date.now();
  const bucket = (offsetMs: number, avg: number) => ({
    time: new Date(now + offsetMs).toISOString(),
    min: avg - 2,
    avg,
    max: avg + 2,
  });
  // Payload A: buckets one/two hours old.
  chartFixture = {
    buckets: [bucket(-2 * 3600_000, 10), bucket(-3600_000, 12)],
    downtime_intervals: [],
    unknown_intervals: [],
    latency_available: true,
  };

  await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
  const chart = page.getByTestId("response-chart");
  await expect(chart).toBeVisible({ timeout: 15_000 });

  interface ChartGeometry {
    path: string;
    xs: number[];
    labels: string[];
    width: number;
  }
  const geometry = (): Promise<ChartGeometry> =>
    chart.evaluate((root) => {
      const d =
        root.querySelector("svg path.path-line")?.getAttribute("d") ?? "";
      const nums = /-?\d+(?:\.\d+)?/g;
      const xs: number[] = [];
      let match: RegExpExecArray | null;
      let isX = true;
      while ((match = nums.exec(d)) !== null) {
        if (isX) xs.push(Number(match[0]));
        isX = !isX;
      }
      const labels = Array.from(
        root.querySelectorAll("svg .x-axis .tick text"),
      ).map((t) => t.textContent ?? "");
      return { path: d, xs, labels, width: root.clientWidth };
    });

  const before = await geometry();
  expect(before.xs.length).toBeGreaterThanOrEqual(2);

  // MOCK (WS): an overall health event is an accepted refresh trigger for the
  // chart. The refreshed payload carries a NEW bucket THREE HOURS AHEAD of the
  // browser clock (the "server clock runs ahead" case).
  chartFixture = {
    buckets: [
      bucket(-2 * 3600_000, 10),
      bucket(-3600_000, 12),
      bucket(3 * 3600_000, 14),
    ],
    downtime_intervals: [],
    unknown_intervals: [],
    latency_available: true,
  };
  live.send("monitor.health", {
    monitor_id: monitor.id,
    projection_version: "1",
    as_of: new Date().toISOString(),
    status: "down",
  });

  await expect
    .poll(async () => (await geometry()).path, { timeout: 10_000 })
    .not.toBe(before.path);
  expect(chartCalls).toBeGreaterThanOrEqual(2);

  const after = await geometry();

  // Sanity: the x axis renders tick labels at all.
  expect(after.labels.length).toBeGreaterThan(0);

  // The fresh future bucket stayed INSIDE the domain: its x coordinate is
  // within the plot bounds (a frozen window end would scale it past the right
  // edge and clip it).
  expect(after.xs.length).toBe(3);
  expect(Math.max(...after.xs)).toBeLessThan(after.width);

  // The window end advanced with the accepted payload: the existing points
  // slid left by ~3h over the 24h axis (a frozen end keeps them pinned).
  // (Tick LABEL strings are not usable here: d3 nice ticks snap to 6h
  // boundaries, so a 3h shift can format to the exact same labels.)
  expect(after.xs[0]).toBeLessThan(before.xs[0] - 20);
});
