import { describe, expect, it } from "vitest";

/** Field names frozen from PROTOCOL.md §§7–8. Keep in lockstep with Go DTOs. */
const healthViewKeys = [
  "monitor_id",
  "status",
  "health_policy",
  "projection_version",
  "as_of",
  "uptime_percent",
  "coverage_percent",
  "known_seconds",
  "unknown_seconds",
  "maintenance_seconds",
  "probe_counts",
  "regions",
] as const;

const regionalHeartbeatKeys = [
  "id",
  "monitor_id",
  "status",
  "ping",
  "message",
  "time",
  "important",
  "probe_id",
  "received_at",
  "assignment_generation",
  "config_revision",
] as const;

const browserEvents = [
  "probe.status",
  "monitor.probe.heartbeat",
  "monitor.probe.status",
  "monitor.health",
  "probe.config.status",
  "probe.command.status",
] as const;

describe("regional probe view contracts", () => {
  it("keeps HTTP heartbeat message separate from browser msg", () => {
    expect(regionalHeartbeatKeys).toContain("message");
    expect(regionalHeartbeatKeys).not.toContain("msg");
  });

  it("freezes HealthView and browser event names", () => {
    expect(healthViewKeys).toContain("probe_counts");
    expect(healthViewKeys).toContain("regions");
    expect(browserEvents).toEqual([
      "probe.status",
      "monitor.probe.heartbeat",
      "monitor.probe.status",
      "monitor.health",
      "probe.config.status",
      "probe.command.status",
    ]);
  });
});
