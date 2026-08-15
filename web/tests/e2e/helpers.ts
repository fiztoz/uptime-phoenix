import { expect, type Page } from "@playwright/test";

export const BASE_URL = process.env.PHOENIX_BASE_URL || "http://127.0.0.1:3100";
export const API_BASE = process.env.PHOENIX_API_URL || BASE_URL;
export const ADMIN_USERNAME = process.env.PHOENIX_E2E_ADMIN || "admin";
export const ADMIN_PASSWORD =
  process.env.PHOENIX_E2E_PASSWORD || "ChangeMe123!";

export function uniqueName(prefix: string): string {
  // crypto is available in Node/Playwright; avoid Math.random for CodeQL.
  const suffix = crypto.randomUUID().replace(/-/g, "").slice(0, 8);
  return `${prefix}-${Date.now()}-${suffix}`;
}

/** Sign in through the real login form and wait for the initial WS snapshot. */
export async function loginViaUI(
  page: Page,
  username = ADMIN_USERNAME,
  password = ADMIN_PASSWORD,
): Promise<void> {
  await page.goto(`${BASE_URL}/login`);
  await expect(page.locator("#login-username")).toBeVisible({
    timeout: 15_000,
  });
  await page.locator("#login-username").fill(username);
  await page.locator("#login-password").fill(password);
  await Promise.all([
    page.waitForURL("**/dashboard", { timeout: 15_000 }),
    page.getByRole("button", { name: "Sign in", exact: true }).click(),
  ]);
  await expect(page.getByRole("link", { name: "Dashboard" })).toBeVisible();
  await expect
    .poll(
      () =>
        page.evaluate(() => {
          const realtime = (
            window as Window & {
              __phoenixRealtime?: { hasMonitorSnapshot: boolean };
            }
          ).__phoenixRealtime;
          return realtime?.hasMonitorSnapshot ?? false;
        }),
      {
        timeout: 15_000,
        message: "initial monitor snapshot should arrive over WebSocket",
      },
    )
    .toBe(true);
}

export async function authToken(page: Page): Promise<string> {
  const token = await page.evaluate(() => localStorage.getItem("phoenix_jwt"));
  if (!token) throw new Error("logged-in page has no phoenix_jwt");
  return token;
}

export interface MonitorView {
  id: number;
  name: string;
  type: string;
  status: string;
  config?: Record<string, unknown>;
  retry_interval?: number;
  max_retries?: number;
  resend_interval?: number;
  tls_ignore?: boolean;
}

/** Create a monitor as setup for a flow that is exercising another UI. */
export async function createHttpMonitorViaApi(
  page: Page,
  token: string,
  name: string,
): Promise<MonitorView> {
  const response = await page.request.post(`${API_BASE}/api/monitors`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      name,
      type: "http",
      active: true,
      interval: 10,
      timeout: 5,
      retry_interval: 5,
      max_retries: 1,
      resend_interval: 0,
      tls_ignore: false,
      accepted_statuscodes: ["200-299"],
      config: { url: `${BASE_URL}/api/health/live`, method: "GET" },
    },
  });
  if (!response.ok()) {
    throw new Error(
      `create monitor failed: ${response.status()} ${await response.text()}`,
    );
  }
  return response.json() as Promise<MonitorView>;
}
