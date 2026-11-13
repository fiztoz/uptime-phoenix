import { expect, test } from "@playwright/test";
import {
  API_BASE,
  BASE_URL,
  authToken,
  createHttpMonitorViaApi,
  loginViaUI,
  uniqueName,
} from "./helpers";

// Regression for #195. ResponseTimeChart's window-end effect read the $state it
// wrote, so each write re-ran it with a later Date.now() and wrote again. When
// one pass took over 1 ms, as on a page with a day of heartbeats, it never
// settled. Svelte then aborted with effect_update_depth_exceeded and the app
// showed "500 Unexpected error".
//
// Making every Date.now() call 1 ms later than the last makes that slow pass
// deterministic. The old effect fails on every load; the fixed one runs once
// per chart payload.
test("response-time chart settles when the clock advances between effect passes (#195)", async ({
  browser,
}) => {
  const context = await browser.newContext();
  await context.addInitScript(() => {
    const real = Date.now.bind(Date);
    let tick = 0;
    Date.now = () => real() + ++tick;
  });
  const page = await context.newPage();
  const loopErrors: string[] = [];
  const collect = (text: string) => {
    if (text.includes("effect_update_depth_exceeded")) loopErrors.push(text);
  };
  page.on("pageerror", (err) => collect(String(err)));
  page.on("console", (msg) => collect(msg.text()));

  try {
    await loginViaUI(page);
    const token = await authToken(page);
    const monitor = await createHttpMonitorViaApi(
      page,
      token,
      uniqueName("Chart loop"),
    );

    // Wait for real heartbeats, so the chart has a payload with buckets.
    await expect
      .poll(
        async () => {
          const res = await page.request.get(
            `${API_BASE}/api/monitors/${monitor.id}/heartbeats?limit=5`,
            { headers: { Authorization: `Bearer ${token}` } },
          );
          if (!res.ok()) return 0;
          const body = await res.json();
          const rows = Array.isArray(body)
            ? body
            : (body.heartbeats ?? body.data ?? []);
          return rows.length;
        },
        { timeout: 45_000, message: "monitor should record heartbeats" },
      )
      .toBeGreaterThan(0);

    await page.goto(`${BASE_URL}/monitors/${monitor.id}`);
    await expect(
      page.getByRole("heading", { name: "Response time" }),
    ).toBeVisible({ timeout: 15_000 });
    // Give a looping effect time to trip Svelte's guard before asserting.
    await page.waitForTimeout(1_500);
    await expect(page.getByText("Unexpected error")).toHaveCount(0);
    expect(loopErrors).toEqual([]);
  } finally {
    await context.close();
  }
});
