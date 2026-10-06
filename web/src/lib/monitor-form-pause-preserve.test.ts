/**
 * Regression tests for GitHub #58 — ordinary edits must preserve the pause
 * state.
 *
 * handleSubmit used to build one body with `active: true` for both create and
 * update, so saving a name change on a paused monitor silently resumed it (and
 * its checks/notifications). The update body must omit `active` entirely:
 * omitted keys keep the stored value (UpdateMonitorRequest in
 * internal/adapters/http/handlers/monitor.go), and a concurrent pause/resume is
 * never overwritten. Resuming stays the explicit Resume action
 * (monitorsApi.resume sends {active: true}).
 */
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  buildMonitorCreateBody,
  buildMonitorUpdateBody,
  type MonitorDraftValues,
} from "./api/monitors";

/** Form values for "change only the name" on an existing paused monitor. */
function pausedMonitorEdit(): MonitorDraftValues {
  return {
    name: "Renamed while paused",
    description: "",
    owner: "",
    inheritGroupOwner: false,
    type: "http",
    interval: 60,
    timeout: 30,
    retryInterval: 60,
    maxRetries: 0,
    resendInterval: 0,
    weight: 2000,
    upsideDown: false,
    tlsIgnore: false,
    certExpiryNotify: false,
    acceptedStatusCodes: "200-299",
    config: { url: "https://example.com" },
    groupId: null,
    proxyId: null,
  };
}

describe("update body preserves pause state (issue #58)", () => {
  test("the submitted update body for a paused monitor carries no active flag", () => {
    const body = buildMonitorUpdateBody(pausedMonitorEdit());
    expect("active" in body).toBe(false);
    expect(body.name).toBe("Renamed while paused");
  });

  test("keeps the ordinary settings in the same body", () => {
    const body = buildMonitorUpdateBody(pausedMonitorEdit());
    expect(body.interval).toBe(60);
    expect(body.timeout).toBe(30);
    expect(body.config).toEqual({ url: "https://example.com" });
    expect(body.accepted_statuscodes).toEqual(["200-299"]);
    expect(body.group_id).toBeNull();
    expect(body.proxy_id).toBeNull();
    expect(body.weight).toBe(2000);
  });

  test("never emits active even for an active monitor's edit", () => {
    const body = buildMonitorUpdateBody({
      ...pausedMonitorEdit(),
      name: "Still active",
    });
    expect("active" in body).toBe(false);
  });

  test("new monitors default to active", () => {
    const body = buildMonitorCreateBody(pausedMonitorEdit());
    expect(body.active).toBe(true);
  });

  test("create has no activation override — the deferred-activation option is gone", () => {
    // issue #60 made the create atomic: the initial assignment set rides the
    // same POST, so there is no paused-create-then-resume flow left to opt
    // into. New monitors run immediately from the one request.
    const withDraft = buildMonitorCreateBody(pausedMonitorEdit(), {
      probe_ids: ["probe-sg"],
      health_policy: "any_down",
    });
    expect(withDraft.active).toBe(true);
    expect(buildMonitorCreateBody(pausedMonitorEdit(), null).active).toBe(true);
    const src = readFileSync(
      new URL("./api/monitors.ts", import.meta.url),
      "utf8",
    );
    expect(src).not.toContain("active?: boolean;\n    initial?:");
    expect(src).not.toMatch(/buildMonitorCreateBody\([\s\S]*?opts\?\.active/);
  });
});

describe("pause/resume stay explicit actions (issue #58)", () => {
  test("the update call site feeds the active-free builder", () => {
    const src = readFileSync(
      new URL("./components/MonitorForm.svelte", import.meta.url),
      "utf8",
    );
    // Line-anchored so a commented-out call cannot satisfy the guard.
    expect(src).toMatch(/^\s*buildMonitorUpdateBody\(values\),\s*$/m);
    // The shared input literal that used to pin active: true is gone.
    expect(src).not.toContain("const input: CreateMonitorInput");
  });

  test("monitorsApi pause/resume still send explicit active flags", () => {
    const src = readFileSync(
      new URL("./api/monitors.ts", import.meta.url),
      "utf8",
    );
    expect(src).toMatch(/async pause\(id: number\)[\s\S]*?\{ active: false \}/);
    expect(src).toMatch(/async resume\(id: number\)[\s\S]*?\{ active: true \}/);
  });
});
