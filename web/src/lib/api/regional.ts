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
  alert_delivery_pending: boolean;
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

export interface MonitorRegionView {
  probe_id: string;
  name: string;
  location: string;
  status: RegionalStatus;
  connection_status: string | null;
  observed_at: string | null;
  received_at: string | null;
  fresh_until: string | null;
  config_sync_status: string | null;
  reason: string | null;
}
export interface MonitorHealthView {
  monitor_id: number;
  status: RegionalStatus;
  health_policy: string;
  projection_version: string;
  as_of: string;
  uptime_percent: number | null;
  coverage_percent: number | null;
  known_seconds: number;
  unknown_seconds: number;
  maintenance_seconds: number;
  probe_counts: {
    assigned: number;
    up: number;
    down: number;
    pending: number;
    unknown: number;
    maintenance: number;
    paused: number;
  };
  regions: MonitorRegionView[];
}
export interface AssignmentInput {
  expected_revision: string;
  probe_ids: string[];
  health_policy: string;
  alert_delivery: string;
  bindings?: Array<{ probe_id: string; kind: string; binding_key: string }>;
}

export interface RegionalAlert {
  source_alert_id: string;
  monitor_id: number;
  probe_id: string;
  probe_name: string;
  location: string;
  assignment_generation: string;
  status: string;
  subject_kind: string;
  reason: string;
  started_at: string;
  acked_at: string | null;
  resolved_at: string | null;
}
export interface RegionalAckReceipt {
  command_id: string;
  status: "pending" | "applied" | "failed" | "expired";
  remote_confirmed: boolean;
}

export const regionalApi = {
  alerts(monitorId: number): Promise<RegionalAlert[]> {
    return api.get(`/monitors/${monitorId}/probe-alerts`);
  },
  acknowledge(
    monitorId: number,
    alertId: string,
    command_id: string,
  ): Promise<RegionalAckReceipt> {
    return api.post(
      `/monitors/${monitorId}/probe-alerts/${encodeURIComponent(alertId)}/ack`,
      { command_id },
    );
  },
  acknowledgement(
    monitorId: number,
    alertId: string,
    commandId: string,
  ): Promise<RegionalAckReceipt> {
    return api.get(
      `/monitors/${monitorId}/probe-alerts/${encodeURIComponent(alertId)}/ack/${encodeURIComponent(commandId)}`,
    );
  },
  health(monitorId: number, hours = 24): Promise<MonitorHealthView> {
    return api.get(`/monitors/${monitorId}/health`, { hours });
  },
  replace(
    monitorId: number,
    input: AssignmentInput,
  ): Promise<MonitorProbeAssignmentsView> {
    return api.put(`/monitors/${monitorId}/probes`, input);
  },
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
