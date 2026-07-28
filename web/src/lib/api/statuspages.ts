/**
 * Status Page CRUD, monitor assignment, incident management API wrappers.
 */
import { api } from "./client";

export type StatusPageDashboardStyle = "full" | "grid" | "pills";

export interface StatusPage {
  id: number;
  title: string;
  slug: string;
  description?: string;
  /** Page logo URL or data:image data-URL. */
  icon?: string;
  /** Browser-tab favicon URL or data:image data-URL. */
  favicon?: string;
  theme: "light" | "dark" | "auto";
  published: boolean;
  // True when the page has a password/access code set. The password itself
  // is write-only and is never returned by the API (see `access_code` below).
  has_access?: boolean;
  footer_text?: string;
  custom_css?: string;
  dashboard_style?: StatusPageDashboardStyle;
  show_tags?: boolean;
  auto_resolve_incidents?: boolean;
  /** When false, public pages hide “Powered by Phoenix” (F3.5). Default true. */
  show_powered_by?: boolean;
  // Optional public SLA target. Null means the uptime history page shows
  // measured percentages without claiming a contractual target.
  sla_target?: number | null;
  created_at: string;
  updated_at: string;
}

export interface CreateStatusPageInput {
  title: string;
  slug: string;
  description?: string;
  icon?: string;
  favicon?: string;
  theme?: "light" | "dark" | "auto";
  published?: boolean;
  // Sets/replaces the page's access password. Write-only: the backend binds
  // this to its `access_code` request field (see CreateSPRequest/UpdateSPRequest
  // in internal/adapters/http/handlers/statuspage.go) and only ever returns
  // `has_access` (never the password) — so this key must stay `access_code`,
  // not `password`, or the value is silently dropped on submit. On update,
  // an explicit empty string removes protection; omission preserves it.
  access_code?: string;
  footer_text?: string;
  custom_css?: string;
  dashboard_style?: StatusPageDashboardStyle;
  show_tags?: boolean;
  auto_resolve_incidents?: boolean;
  show_powered_by?: boolean;
  // Set to 0 on update to clear the optional SLA target.
  sla_target?: number;
}

export interface UpdateStatusPageInput extends Partial<CreateStatusPageInput> {}

export type IncidentTimelineStatus =
  | "investigating"
  | "identified"
  | "monitoring"
  | "resolved";

export interface IncidentUpdate {
  id: number;
  incident_id: number;
  status_page_id: number;
  status: IncidentTimelineStatus;
  content: string;
  created_at: string;
}

export interface Incident {
  id: number;
  status_page_id: number;
  title: string;
  content: string;
  style: "info" | "warning" | "danger" | "success";
  // Domain resolution is Active=false; there is no resolved_at timestamp.
  // Admin IncidentView uses `active`; public PublicIncidentView does too.
  active: boolean;
  pinned?: boolean;
  created_at: string;
  // Present on public payloads when a resolved timeline update exists; prefer
  // `active` for UI state.
  resolved_at?: string;
  // Markdown incident timeline, oldest first.
  updates: IncidentUpdate[];
}

export interface StatusPageCNAME {
  id: number;
  status_page_id: number;
  domain: string;
}

/**
 * Wire shape of GET /status-pages/:id/monitors (SPMonitorView in
 * internal/adapters/http/handlers/statuspage.go). This is the raw
 * status_page_monitors link row — it carries `monitor_id` and
 * `display_order` only, NOT the monitor's name/type/status. Callers that
 * need those must join against `monitorsApi.list()` by `monitor_id`.
 */
export interface StatusPageMonitorLink {
  id: number;
  status_page_id: number;
  monitor_id: number;
  display_order: number;
}

export interface CreateIncidentInput {
  title: string;
  content: string;
  style?: "info" | "warning" | "danger" | "success";
}

export interface CreateIncidentUpdateInput {
  status: IncidentTimelineStatus;
  content: string;
}

export interface StatusPageSubscriber {
  id: number;
  status_page_id: number;
  email: string;
  active: boolean;
  confirmed_at?: string;
  created_at: string;
}

export interface SubscriptionChannel {
  notification_id: number;
}

export interface UptimeHistoryPeriod {
  label: string;
  start_date: string;
  end_date: string;
  uptime_percent: number | null;
  complete: boolean;
}

export interface PublicStatusResponse {
  status_page: StatusPage;
  // True when PUBLIC_URL is set and the page has an active SMTP channel.
  // Never exposes provider details.
  subscriptions_available: boolean;
  monitors: Array<{
    id: number;
    name: string;
    type: string;
    // Backend (internal/core/services/statuspage_service.go) sends the domain
    // heartbeat statuses only — it never emits "paused" on a public status page.
    status: "up" | "down" | "pending" | "maintenance";
    uptime_percent?: number | null;
    // 90-day data for bar, UTC dates, oldest first, always exactly 90 entries.
    // "none" marks a day with no checks at all.
    uptime_data: Array<{
      date: string;
      status: "up" | "down" | "pending" | "maintenance" | "none";
    }>;
    // Calendar summaries are newest-first. No-data periods use null rather
    // than inventing 0% or 100%; current periods have complete=false.
    uptime_history: {
      monthly: UptimeHistoryPeriod[];
      quarterly: UptimeHistoryPeriod[];
    };
    // Response-time series for the per-monitor chart, bucketed server-side
    // (internal/core/services/statuspage_service.go: PublicMonitorChart,
    // fixed last 24h — no public range API). Same shape as the authenticated
    // chart_aggregate API (see ResponseTimeChart's ChartPayload). Public UI
    // renders with showRangeSelector={false}. Backend always sends a `chart`
    // object with (possibly empty) arrays — kept optional/nullable here
    // defensively so the UI still gates on presence rather than fabricating points.
    chart?: {
      buckets: Array<{ time: string; min: number; avg: number; max: number }>;
      downtime_intervals: Array<{ start: string; end: string }>;
    } | null;
    // Present only when cached TLS data exists — never invent zeros.
    cert_expiry_date?: string | null;
    cert_days_left?: number | null;
  }>;
  incidents: Incident[];
}

export const statusPagesApi = {
  async list(): Promise<StatusPage[]> {
    return api.get<StatusPage[]>("/status-pages");
  },

  async get(id: number): Promise<StatusPage> {
    return api.get<StatusPage>(`/status-pages/${id}`);
  },

  async create(input: CreateStatusPageInput): Promise<StatusPage> {
    return api.post<StatusPage>("/status-pages", input);
  },

  async update(id: number, input: UpdateStatusPageInput): Promise<StatusPage> {
    return api.put<StatusPage>(`/status-pages/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/status-pages/${id}`);
  },

  // Monitor assignment
  async listMonitors(id: number): Promise<StatusPageMonitorLink[]> {
    return api.get(`/status-pages/${id}/monitors`);
  },

  /**
   * `displayOrder` is optional; the backend defaults any value <= 0 to 1000
   * (see StatusPageService.AddMonitor). There is no update/reorder endpoint —
   * changing an existing assignment's order requires remove + re-add.
   */
  async addMonitor(
    id: number,
    monitorId: number,
    displayOrder?: number,
  ): Promise<void> {
    return api.post(`/status-pages/${id}/monitors`, {
      monitor_id: monitorId,
      ...(displayOrder != null ? { display_order: displayOrder } : {}),
    });
  },

  async removeMonitor(id: number, monitorId: number): Promise<void> {
    return api.del(`/status-pages/${id}/monitors/${monitorId}`);
  },

  /**
   * Atomic reorder: replaces ALL monitor assignments in one transaction.
   * Pass monitor IDs in the desired display order. Assignments for monitors
   * not in the list are removed.
   */
  async reorderMonitors(id: number, monitorIds: number[]): Promise<void> {
    return api.put(`/status-pages/${id}/monitors`, { monitor_ids: monitorIds });
  },

  // Incidents
  async listIncidents(id: number): Promise<Incident[]> {
    return api.get<Incident[]>(`/status-pages/${id}/incidents`);
  },

  async createIncident(
    id: number,
    input: CreateIncidentInput,
  ): Promise<Incident> {
    return api.post<Incident>(`/status-pages/${id}/incidents`, input);
  },

  async updateIncident(
    statusPageId: number,
    incidentId: number,
    input: Partial<CreateIncidentInput>,
  ): Promise<Incident> {
    return api.put<Incident>(
      `/status-pages/${statusPageId}/incidents/${incidentId}`,
      input,
    );
  },

  async listIncidentUpdates(
    statusPageId: number,
    incidentId: number,
  ): Promise<IncidentUpdate[]> {
    return api.get<IncidentUpdate[]>(
      `/status-pages/${statusPageId}/incidents/${incidentId}/updates`,
    );
  },

  async createIncidentUpdate(
    statusPageId: number,
    incidentId: number,
    input: CreateIncidentUpdateInput,
  ): Promise<IncidentUpdate> {
    return api.post<IncidentUpdate>(
      `/status-pages/${statusPageId}/incidents/${incidentId}/updates`,
      input,
    );
  },

  /** Marks an incident inactive via POST …/resolve (sets Active=false). */
  async resolveIncident(
    statusPageId: number,
    incidentId: number,
  ): Promise<void> {
    return api.post(
      `/status-pages/${statusPageId}/incidents/${incidentId}/resolve`,
    );
  },

  async deleteIncident(
    statusPageId: number,
    incidentId: number,
  ): Promise<void> {
    return api.del(`/status-pages/${statusPageId}/incidents/${incidentId}`);
  },

  // Custom domains (CNAME aliases) — the working multi-domain model.
  async listCNAMEs(statusPageId: number): Promise<StatusPageCNAME[]> {
    return api.get<StatusPageCNAME[]>(`/status-pages/${statusPageId}/cnames`);
  },

  async addCNAME(
    statusPageId: number,
    domain: string,
  ): Promise<StatusPageCNAME> {
    return api.post<StatusPageCNAME>(`/status-pages/${statusPageId}/cnames`, {
      domain,
    });
  },

  async removeCNAME(statusPageId: number, cnameId: number): Promise<void> {
    return api.del(`/status-pages/${statusPageId}/cnames/${cnameId}`);
  },

  // Public endpoint (no auth)
  async getPublic(slug: string): Promise<PublicStatusResponse> {
    return api.get<PublicStatusResponse>(`/status/${slug}`);
  },

  // The anonymous GET returns metadata only when has_access is true. This
  // endpoint verifies the write-only access code and returns the full payload.
  async verifyAccess(
    slug: string,
    accessCode: string,
  ): Promise<PublicStatusResponse> {
    return api.post<PublicStatusResponse>(`/status/${slug}/verify-access`, {
      access_code: accessCode,
    });
  },

  // --- Email subscriptions (Sprint C) ------------------------------------

  /** Public double-opt-in subscribe. Always 202 on the post-validation path. */
  async subscribe(
    slug: string,
    email: string,
    accessCode?: string,
  ): Promise<void> {
    await api.post(`/status/${slug}/subscribers`, {
      email,
      ...(accessCode ? { access_code: accessCode } : {}),
    });
  },

  async confirmSubscription(token: string): Promise<void> {
    await api.post("/status/subscriptions/confirm", { token });
  },

  async unsubscribe(token: string): Promise<void> {
    await api.post("/status/subscriptions/unsubscribe", { token });
  },

  async listSubscribers(statusPageId: number): Promise<StatusPageSubscriber[]> {
    return api.get<StatusPageSubscriber[]>(
      `/status-pages/${statusPageId}/subscribers`,
    );
  },

  async removeSubscriber(
    statusPageId: number,
    subscriberId: number,
  ): Promise<void> {
    return api.del(`/status-pages/${statusPageId}/subscribers/${subscriberId}`);
  },

  async getSubscriptionChannel(
    statusPageId: number,
  ): Promise<SubscriptionChannel | null> {
    return api.get<SubscriptionChannel | null>(
      `/status-pages/${statusPageId}/subscription-channel`,
    );
  },

  async setSubscriptionChannel(
    statusPageId: number,
    notificationId: number,
  ): Promise<SubscriptionChannel> {
    return api.put<SubscriptionChannel>(
      `/status-pages/${statusPageId}/subscription-channel`,
      { notification_id: notificationId },
    );
  },

  async clearSubscriptionChannel(statusPageId: number): Promise<void> {
    return api.del(`/status-pages/${statusPageId}/subscription-channel`);
  },
};
