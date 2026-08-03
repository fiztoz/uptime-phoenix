/**
 * Monitor statistics API.
 */
import { api } from "./client";

export interface MonitorStats {
  current_ping_ms: number;
  avg_ping_24h: number;
  uptime_24h: number | null;
  uptime_30d: number | null;
  cert_expiry_date?: string;
  cert_days_left?: number;
}

export const statsApi = {
  async get(monitorId: number): Promise<MonitorStats> {
    return api.get<MonitorStats>(`/monitors/${monitorId}/stats`);
  },
};
