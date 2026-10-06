/**
 * Regression tests for GitHub #60 — create remote-only monitors with their
 * initial assignments atomically.
 *
 * The create body may carry `probe_ids` / `health_policy` / `probe_bindings`
 * (CreateMonitorRequest in internal/adapters/http/handlers/monitor.go); when
 * present the backend commits monitor + desired set through
 * CreateWithAssignments in one transaction, so a rejected set leaves no
 * monitor behind. A default set (["local"], any_down, no bindings) is omitted
 * and takes the backend local-default path unchanged. These fields are
 * create-only — PUT /api/monitors/:id has no assignment fields.
 */
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import {
  buildMonitorCreateBody,
  buildMonitorUpdateBody,
  type MonitorDraftValues,
  type MonitorInitialAssignments,
} from "./api/monitors";

function draft(): MonitorDraftValues {
  return {
    name: "Remote-only API",
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

describe("create body carries the initial assignment set (issue #60)", () => {
  test("a remote-only set rides the create request with the exact wire keys", () => {
    const initial: MonitorInitialAssignments = {
      probe_ids: ["probe-sg"],
      health_policy: "all_down",
      probe_bindings: [
        { probe_id: "probe-sg", kind: "docker_socket", binding_key: "edge-1" },
      ],
    };
    const body = buildMonitorCreateBody(draft(), initial);
    expect(body.probe_ids).toEqual(["probe-sg"]);
    expect(body.health_policy).toBe("all_down");
    expect(body.probe_bindings).toEqual([
      { probe_id: "probe-sg", kind: "docker_socket", binding_key: "edge-1" },
    ]);
    expect(body.probe_ids).not.toContain("local");
    // Binding entry keys match MonitorProbeBindingRequest json tags.
    expect(Object.keys(body.probe_bindings![0])).toEqual([
      "probe_id",
      "kind",
      "binding_key",
    ]);
  });

  test("a default set is omitted so the local-default create path is unchanged", () => {
    const body = buildMonitorCreateBody(draft(), {
      probe_ids: ["local"],
      health_policy: "any_down",
    });
    expect("probe_ids" in body).toBe(false);
    expect("health_policy" in body).toBe(false);
    expect("probe_bindings" in body).toBe(false);
  });

  test("no editor draft (non-admin) omits the assignment fields entirely", () => {
    const body = buildMonitorCreateBody(draft(), null);
    expect("probe_ids" in body).toBe(false);
    expect("health_policy" in body).toBe(false);
    expect("probe_bindings" in body).toBe(false);
    expect(body.active).toBe(true);
  });

  test("empty bindings are omitted even for a non-default set", () => {
    const body = buildMonitorCreateBody(draft(), {
      probe_ids: ["probe-sg", "probe-fra"],
      health_policy: "any_down",
      probe_bindings: [],
    });
    expect(body.probe_ids).toEqual(["probe-sg", "probe-fra"]);
    expect("probe_bindings" in body).toBe(false);
  });

  test("an all-local set with a non-default policy is still explicit", () => {
    const body = buildMonitorCreateBody(draft(), {
      probe_ids: ["local"],
      health_policy: "all_down",
    });
    expect(body.probe_ids).toEqual(["local"]);
    expect(body.health_policy).toBe("all_down");
  });

  test("update bodies never carry create-only assignment fields", () => {
    const body = buildMonitorUpdateBody(draft());
    expect("probe_ids" in body).toBe(false);
    expect("health_policy" in body).toBe(false);
    expect("probe_bindings" in body).toBe(false);
  });
});

describe("MonitorForm create flow wiring (issue #60)", () => {
  const src = readFileSync(
    new URL("./components/MonitorForm.svelte", import.meta.url),
    "utf8",
  );

  test("the editor draft is requested and submitted with the create POST", () => {
    // Line-anchored so commented-out calls cannot satisfy the guards.
    expect(src).toMatch(
      /^\s*const initial = assignmentEditor\?\.initialAssignments\(\) \?\? null;\s*$/m,
    );
    expect(src).toMatch(/^\s*buildMonitorCreateBody\(values, initial\),\s*$/m);
  });

  test("create is exactly one POST — no pause/resume/delete compensation", () => {
    expect(src.match(/await monitorsApi\.create\(/g)).toHaveLength(1);
    // The deferred-activation fallback (paused create + region save + resume
    // + rollback delete) is gone entirely.
    expect(src).not.toMatch(/monitorsApi\.(remove|resume|pause)\(/);
    expect(src).not.toContain("assignmentEditor.save(created.id)");
    expect(src).not.toContain("{ active: false }");
  });

  test("the assignment editor is typed as the real component instance", () => {
    expect(src).toContain("ReturnType<typeof ProbeAssignments>");
    expect(src).not.toContain("AssignmentEditorExports");
    expect(src).not.toContain("initialAssignments?:");
  });
});

describe("ProbeAssignments draft export (issue #60)", () => {
  const src = readFileSync(
    new URL("./components/ProbeAssignments.svelte", import.meta.url),
    "utf8",
  );

  test("exposes a required typed draft accessor", () => {
    expect(src).toMatch(
      /^\s*export function initialAssignments\(\): MonitorInitialAssignments \{\s*$/m,
    );
  });

  test("disabled probes answer the local default without any explicit API", () => {
    // Line-anchored: the local-default draft lets the create POST omit the
    // set and take the backend local-default path — no PUT .../probes needed.
    expect(src).toMatch(
      /^\s*if \(unavailable\) return \{ probe_ids: \["local"\], health_policy: "any_down" \};\s*$/m,
    );
  });

  test("refuses to answer while loading, on error, or with an empty selection", () => {
    expect(src).toMatch(
      /^\s*if \(loading\) throw new Error\(m\.probes_loading\(\)\);\s*$/m,
    );
    expect(src).toMatch(/^\s*if \(error\) throw new Error\(error\);\s*$/m);
    expect(src).toMatch(/^\s*if \(selected\.length === 0\) \{\s*$/m);
  });

  test("carries only the bindings of selected probes", () => {
    expect(src).toMatch(
      /^\s*probe_bindings: selected\.flatMap\(\(id\) => \{\s*$/m,
    );
  });
});
