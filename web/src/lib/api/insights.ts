import { api } from "./client";

export type InsightsPeriod = "24h" | "7d" | "30d" | "90d";
export type InsightsMetric =
  | "availability"
  | "outages"
  | "downtime"
  | "latency"
  | "flapping";
export type InsightsQualification = "qualified" | "insufficient_data";

export interface InsightsRow {
  monitor_id: number;
  monitor_name: string;
  monitor_type: string;
  group_id: number | null;
  availability_percent: number | null;
  outage_count: number;
  downtime_seconds: number;
  flap_count: number;
  latency_avg_ms: number | null;
  latency_sample_count: number;
  coverage_percent: number;
  qualification: InsightsQualification;
}

export interface InsightsResponse {
  from: string;
  to: string;
  period: InsightsPeriod;
  metric: InsightsMetric;
  coverage_basis: "observation_based";
  rows: InsightsRow[];
}

export interface InsightsQuery {
  period: InsightsPeriod;
  metric: InsightsMetric;
  type?: string;
  group_id?: number;
}

export const insightsApi = {
  async list(query: InsightsQuery): Promise<InsightsResponse> {
    const params: Record<string, string | number | boolean> = {
      period: query.period,
      metric: query.metric,
    };
    if (query.type) params.type = query.type;
    if (query.group_id != null) params.group_id = query.group_id;
    return api.get<InsightsResponse>("/insights", params);
  },
};
