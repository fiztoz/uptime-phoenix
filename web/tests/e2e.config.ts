import { defineConfig, devices } from "@playwright/test";

const phoenixBase = process.env.PHOENIX_BASE_URL;
const defaultBase = "http://127.0.0.1:3100";

export default defineConfig({
  testDir: "./e2e",
  testMatch: "**/*.spec.ts",
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: 1,
  timeout: 60_000,
  reporter: "list",
  use: {
    baseURL: phoenixBase || defaultBase,
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: phoenixBase
    ? undefined
    : {
        command: "sh tests/start-e2e-server.sh",
        url: `${defaultBase}/api/health/live`,
        reuseExistingServer: false,
        timeout: 120_000,
        cwd: process.cwd(),
      },
});
