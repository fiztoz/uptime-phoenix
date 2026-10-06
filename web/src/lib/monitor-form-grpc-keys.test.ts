/**
 * Regression tests for GitHub #59 — gRPC form fields must use the checker's
 * config contract.
 *
 * The gRPC checker (internal/adapters/checker/grpc.go) requires `url` and
 * reads `service_name`, `tls`, `timeout`. The form used to define `hostname`,
 * `service`, `timeout`, so a fresh UI submission failed server validation with
 * `invalid config: url is required` and API-created monitors never populated
 * the form fields.
 */
import { describe, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { migrateLegacyGrpcConfig, monitorTypeConfig } from "./monitor-types";

/** The checker's config keys — keep in lockstep with internal/adapters/checker/grpc.go. */
const CHECKER_KEYS = ["url", "service_name", "tls", "timeout"];

describe("gRPC monitor form metadata (issue #59)", () => {
  const grpc = monitorTypeConfig.grpc;

  test("defines exactly the checker's config keys", () => {
    expect(grpc.fields.map((field) => field.key)).toEqual(CHECKER_KEYS);
  });

  test("requires the target under the checker's `url` key", () => {
    const url = grpc.fields.find((field) => field.key === "url");
    expect(url?.required).toBe(true);
  });

  test("sends a nonempty service name under `service_name`", () => {
    const service = grpc.fields.find((field) => field.key === "service_name");
    expect(service?.required).toBeFalsy();
    expect(service?.key).toBe("service_name");
  });

  test("exposes `tls` as a boolean defaulting to off", () => {
    const tls = grpc.fields.find((field) => field.key === "tls");
    expect(tls?.type).toBe("checkbox");
    expect(tls?.default).toBe(false);
  });

  test("keeps the numeric `timeout` key", () => {
    const timeout = grpc.fields.find((field) => field.key === "timeout");
    expect(timeout?.type).toBe("number");
  });
});

describe("migrateLegacyGrpcConfig (issue #59 compatibility)", () => {
  test("folds legacy hostname/service into url/service_name", () => {
    const cfg = migrateLegacyGrpcConfig({
      hostname: "grpc.example.com:50051",
      service: "my.package.Service",
      timeout: 10,
    });
    expect(cfg).toEqual({
      hostname: "grpc.example.com:50051",
      service: "my.package.Service",
      url: "grpc.example.com:50051",
      service_name: "my.package.Service",
      timeout: 10,
    });
  });

  test("canonical values always win over legacy ones", () => {
    const cfg = migrateLegacyGrpcConfig({
      url: "canonical:50051",
      hostname: "legacy:50051",
      service_name: "canonical.Service",
      service: "legacy.Service",
    });
    expect(cfg.url).toBe("canonical:50051");
    expect(cfg.service_name).toBe("canonical.Service");
  });

  test("fills empty canonical values but never clears them", () => {
    const cfg = migrateLegacyGrpcConfig({
      url: "",
      hostname: "grpc.example.com:50051",
      service_name: "kept.Service",
      service: "legacy.Service",
    });
    expect(cfg.url).toBe("grpc.example.com:50051");
    expect(cfg.service_name).toBe("kept.Service");
  });

  test("ignores non-string legacy values and leaves other keys alone", () => {
    const cfg = migrateLegacyGrpcConfig({
      hostname: 50051,
      service: null,
      tls: true,
      timeout: 10,
    });
    expect("url" in cfg).toBe(false);
    expect("service_name" in cfg).toBe(false);
    expect(cfg).toEqual({
      hostname: 50051,
      service: null,
      tls: true,
      timeout: 10,
    });
  });

  test("is wired into MonitorForm.buildInitialConfig", () => {
    const src = readFileSync(
      new URL("./components/MonitorForm.svelte", import.meta.url),
      "utf8",
    );
    // Call-site guard: the fold runs for grpc configs when seeding the form.
    // Line-anchored so a commented-out call cannot satisfy it.
    expect(src).toMatch(/^\s*migrateLegacyGrpcConfig\(cfg\);\s*$/m);
    expect(src).toMatch(
      /^\s*if \(initialMonitor\?\.type === "grpc" \|\| !initialMonitor\) \{\s*$/m,
    );
  });
});
