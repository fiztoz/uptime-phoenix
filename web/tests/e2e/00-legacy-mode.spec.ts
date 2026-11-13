import { expect, test } from "@playwright/test";
import { API_BASE, authToken, loginViaUI } from "./helpers";

// M6 matrix T33: with PHOENIX_E2E_PROBES=false the server starts as a legacy
// local-only install. This guard proves the mode really is off, so the
// legacy journey specs (01-08) that run next are not passing on a probe-
// enabled hub by accident.
const legacy = process.env.PHOENIX_E2E_PROBES === "false";

test("legacy local-only install keeps the probe surface disabled", async ({
  page,
}) => {
  test.skip(!legacy, "runs only with PHOENIX_E2E_PROBES=false");
  await loginViaUI(page);
  const token = await authToken(page);
  const response = await page.request.get(`${API_BASE}/api/probes`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(response.status()).toBe(503);
  expect(await response.json()).toMatchObject({ code: "probes_disabled" });
});
