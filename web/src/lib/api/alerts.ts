import { api } from "./client";

export type AlertStatus = "firing" | "acked" | "resolved";

/** F2.3 ladder progress for an alert. Absent when no policy applies. */
export interface AlertEscalation {
  status: "pending" | "done" | "canceled";
  /** The step not yet sent; 1-based. Step 0 is the initial DOWN notification. */
  next_step: number;
  next_run_at: string;
}

export interface Alert {
  id: number;
  monitor_id: number;
  status: AlertStatus;
  message: string;
  fired_at: string;
  acked_at?: string | null;
  acked_by_user_id?: number | null;
  resolved_at?: string | null;
  created_at: string;
  updated_at: string;
  escalation?: AlertEscalation | null;
}

export interface AlertListParams {
  status?: string;
  open?: boolean;
  monitor_id?: number;
  limit?: number;
  offset?: number;
}

export const alertsApi = {
  async list(params: AlertListParams = {}): Promise<Alert[]> {
    const q = new URLSearchParams();
    if (params.status) q.set("status", params.status);
    if (params.open) q.set("open", "1");
    if (params.monitor_id != null)
      q.set("monitor_id", String(params.monitor_id));
    if (params.limit != null) q.set("limit", String(params.limit));
    if (params.offset != null) q.set("offset", String(params.offset));
    const qs = q.toString();
    return api.get<Alert[]>(`/alerts${qs ? `?${qs}` : ""}`);
  },

  async get(id: number): Promise<Alert> {
    return api.get<Alert>(`/alerts/${id}`);
  },

  async acknowledge(id: number): Promise<Alert> {
    return api.post<Alert>(`/alerts/${id}/ack`, {});
  },

  /** Public deep-link ack — no session required. */
  async acknowledgeByToken(token: string): Promise<Alert> {
    return api.post<Alert>("/alerts/ack-by-token", { token });
  },
};
