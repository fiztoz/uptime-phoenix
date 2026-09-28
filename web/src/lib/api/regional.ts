/**
 * M5 regional API client and TypeScript contracts.
 *
 * These types mirror the frozen JSON fixtures checked in beside the Go views —
 * internal/adapters/http/handlers/testdata/m5/{assignments,health,regional_history,regional_chart}.json
 * and internal/adapters/probe/testdata/v1/valid/http-regional-heartbeat.json.
 * Keep them in lockstep with those fixtures and the json tags in
 * monitor_regional.go / monitor_regional_history.go. Revisions and generations
 * are decimal STRINGS (they deliberately exceed 2^53 - 1): never parse them
 * into a JS number.
 */
import { api } from "./client.js";

/** Regional statuses: lowercase, UNKNOWN stays visible. */
export type RegionalStatus =
  | "up"
  | "down"
  | "pending"
  | "maintenance"
  | "unknown";

export interface MonitorProbeBindingView {
  kind: string;
  binding_key: string;
}

export interface MonitorProbeAssignmentView {
  probe_id: string;
  name: string;
  location: string;
  generation: string;
  desired_config_revision: string | null;
  applied_config_revision: string | null;
  sync_status: string | null;
  bindings: MonitorProbeBindingView[];
}

export interface MonitorProbeAssignmentsView {
  revision: string;
  health_policy: string;
  alert_delivery: string;
  assignments: MonitorProbeAssignmentView[];
}

/** One regional history row (frozen M0 shape; keeps message, never msg). */
export interface RegionalHeartbeat {
  id: number;
  monitor_id: number;
  status: RegionalStatus;
  ping: number;
  message: string;
  time: string;
  important: boolean;
  probe_id: string;
  received_at: string;
  assignment_generation: string;
  config_revision: string;
}

export interface RegionalChartPayload {
  buckets: Array<{ time: string; min: number; avg: number; max: number }>;
  downtime_intervals: Array<{ start: string; end: string }>;
  unknown_intervals: Array<{ start: string; end: string }>;
}

export interface RegionalHistoryOptions {
  hours?: number;
  limit?: number;
  order?: "asc" | "desc";
  important?: boolean;
}

export const regionalApi = {
  /** Desired assignment set with safe labels (monitor-scoped, authorized). */
  assignments(monitorId: number): Promise<MonitorProbeAssignmentsView> {
    return api.get(`/monitors/${monitorId}/probes`);
  },

  /** Relationship-checked regional history for one probe. */
  history(
    monitorId: number,
    probeId: string,
    options: RegionalHistoryOptions = {},
  ): Promise<RegionalHeartbeat[]> {
    const { hours = 24, limit, order, important } = options;
    const params: Record<string, string | number | boolean> = { hours };
    if (limit != null) params.limit = limit;
    if (order) params.order = order;
    if (important != null) params.important = important;
    return api.get(
      `/monitors/${monitorId}/probes/${encodeURIComponent(probeId)}/heartbeats`,
      params,
    );
  },

  /** Regional chart: measured latency for this probe plus downtime/unknown intervals. */
  chart(
    monitorId: number,
    probeId: string,
    hours = 24,
  ): Promise<RegionalChartPayload> {
    return api.get(
      `/monitors/${monitorId}/probes/${encodeURIComponent(probeId)}/heartbeats/chart`,
      { hours },
    );
  },
};
