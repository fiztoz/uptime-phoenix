import { api } from "./client";
import type { Heartbeat } from "$lib/monitor-types";

// Canonical Status/Heartbeat live in monitor-types.ts (single source of truth,
// matches the REST API's heartbeatView JSON). Re-exported here so existing
// `import type { Heartbeat } from "$lib/api/heartbeats.js"` call sites keep working.
export type { Status } from "$lib/monitor-types";
export type { Heartbeat };

export interface HeartbeatListOptions {
  hours?: number;
  limit?: number;
  order?: "asc" | "desc";
  important?: boolean;
}

export const heartbeatsApi = {
  /** Get heartbeats for a monitor (default: last 24 hours). */
  async list(monitorId: number, hours = 24): Promise<Heartbeat[]> {
    return heartbeatsApi.listOptions(monitorId, { hours });
  },

  /** Get heartbeats with full query options. */
  async listOptions(
    monitorId: number,
    options: HeartbeatListOptions = {},
  ): Promise<Heartbeat[]> {
    const { hours = 24, limit, order, important } = options;
    const params: Record<string, string | number | boolean> = { hours };
    if (limit != null) params.limit = limit;
    if (order) params.order = order;
    if (important != null) params.important = important;
    return api.get<Heartbeat[]>(`/monitors/${monitorId}/heartbeats`, params);
  },

  /** Delete all heartbeats (clear status/response history) for a monitor. */
  async clear(monitorId: number): Promise<void> {
    return api.del(`/monitors/${monitorId}/heartbeats`);
  },

  /** Server-side chart buckets + downtime intervals (Go chart_aggregate). */
  async chart(
    monitorId: number,
    hours = 24,
  ): Promise<{
    buckets: Array<{ time: string; min: number; avg: number; max: number }>;
    downtime_intervals: Array<{ start: string; end: string }>;
  }> {
    return api.get(`/monitors/${monitorId}/heartbeats/chart`, { hours });
  },
};
