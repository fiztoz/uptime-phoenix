/// <reference types="bun-types" />
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "bun:test";
import { probeEventMatches } from "./probe-detail-events";

// Wire shapes frozen from internal/adapters/ws/regional.go regionalWirePayload
// and internal/adapters/http/handlers/regional_browser.go publishFleet.
const statusPayload = {
  id: "probe-1",
  key: "vm-sg",
  name: "Singapore",
  location: "sg",
  enabled: true,
  connection_status: "disconnected",
  execution_status: "idle",
  last_seen_at: "2026-01-01T02:00:00Z",
  revision: "4",
};
const configStatusPayload = {
  probe_id: "probe-1",
  revision: "4",
  status: "applied",
  errors: [] as string[],
};

describe("probeEventMatches", () => {
  // GitHub #61: probe.status carries `id`, probe.config.status carries
  // `probe_id`. A shared probe_id-only selector dropped every status event.
  test("probe.status matches by its id field", () => {
    expect(probeEventMatches("probe.status", statusPayload, "probe-1")).toBe(
      true,
    );
  });

  test("probe.status does not borrow the config-status identifier", () => {
    expect(
      probeEventMatches("probe.status", { probe_id: "probe-1" }, "probe-1"),
    ).toBe(false);
  });

  test("probe.config.status matches by its probe_id field", () => {
    expect(
      probeEventMatches("probe.config.status", configStatusPayload, "probe-1"),
    ).toBe(true);
  });

  test("probe.config.status does not borrow the status identifier", () => {
    expect(
      probeEventMatches("probe.config.status", { id: "probe-1" }, "probe-1"),
    ).toBe(false);
  });

  test("ignores events for other probes", () => {
    expect(probeEventMatches("probe.status", statusPayload, "probe-2")).toBe(
      false,
    );
    expect(
      probeEventMatches("probe.config.status", configStatusPayload, "probe-2"),
    ).toBe(false);
  });

  test("ignores malformed payloads", () => {
    expect(probeEventMatches("probe.status", null, "probe-1")).toBe(false);
    expect(probeEventMatches("probe.status", "probe-1", "probe-1")).toBe(false);
    expect(probeEventMatches("probe.status", {}, "probe-1")).toBe(false);
    expect(probeEventMatches("probe.status", { id: "" }, "probe-1")).toBe(
      false,
    );
    expect(probeEventMatches("probe.status", { id: 42 }, "probe-1")).toBe(
      false,
    );
    expect(probeEventMatches("probe.config.status", {}, "probe-1")).toBe(false);
    expect(probeEventMatches("probe.status", statusPayload, "")).toBe(false);
  });

  test("matches the local probe identity", () => {
    expect(
      probeEventMatches(
        "probe.status",
        { ...statusPayload, id: "local" },
        "local",
      ),
    ).toBe(true);
  });
});

describe("probe detail page wiring", () => {
  const source = readFileSync(
    join(
      import.meta.dir,
      "..",
      "routes",
      "(admin)",
      "probes",
      "[id]",
      "+page.svelte",
    ),
    "utf8",
  );

  test("each event type is matched with its own identifier contract", () => {
    expect(source).toContain('probeEventMatches("probe.status"');
    expect(source).toContain('probeEventMatches("probe.config.status"');
  });
});

describe("probe list page wiring (sister list, GitHub #61)", () => {
  const source = readFileSync(
    join(import.meta.dir, "..", "routes", "(admin)", "probes", "+page.svelte"),
    "utf8",
  );

  test("each event type is matched with its own identifier contract", () => {
    expect(source).toContain('probeEventMatches("probe.status"');
    expect(source).toContain('probeEventMatches("probe.config.status"');
  });

  test("the shared probe_id-only selector is gone", () => {
    expect(source).not.toContain('"probe_id" in payload');
  });
});
