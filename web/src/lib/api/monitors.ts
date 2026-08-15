/**
 * Monitor CRUD API wrappers.
 */
import { api } from "./client";
import type { Monitor } from "$lib/stores/ws.svelte.ts";

export interface CreateMonitorInput {
  name: string;
  description?: string;
  /** Informational service/team contact; unrelated to the creating user. */
  owner?: string;
  /** Prefer group (and ancestor) contact over owner when true. */
  inherit_group_owner?: boolean;
  type: string;
  interval: number;
  timeout: number;
  /** Seconds between retries after a failed check. */
  retry_interval?: number;
  /** Retries before marking the monitor DOWN. */
  max_retries?: number;
  /** Re-notify every N consecutive failures (0 = notify once). */
  resend_interval?: number;
  /** Flip UP/DOWN interpretation of the check result. */
  upside_down?: boolean;
  /**
   * HTTP: skip TLS certificate verification — insecure. Top-level sibling of
   * `config` (see internal/adapters/http/handlers/monitor.go), honored by
   * checker/http.go via InsecureSkipVerify.
   */
  tls_ignore?: boolean;
  /**
   * HTTP: opt into certificate-expiry alerts at fixed 30/14/7 day thresholds.
   * Top-level wire field `cert_expiry_notify` (default false).
   */
  cert_expiry_notify?: boolean;
  config: Record<string, unknown>;
  /**
   * HTTP: accepted status code ranges, e.g. ["200-299", "301"]. Sent as a
   * top-level sibling of `config` — the backend does NOT read this from inside
   * config (see internal/adapters/http/handlers/monitor.go CreateMonitorRequest).
   */
  accepted_statuscodes?: string[];
  active?: boolean;
  /**
   * Files this monitor under a monitor GROUP (folder) — see
   * $lib/api/monitorGroups.ts. On update, omit the key to leave the folder
   * unchanged; send `null` to pull the monitor out (top-level). See
   * internal/adapters/http/handlers/monitor.go CreateMonitorRequest /
   * UpdateMonitorRequest `group_id`.
   */
  group_id?: number | null;
  /**
   * Routes this monitor's checks through an outbound proxy owned by the
   * same user (see web/src/lib/api/proxies.ts). On update, omit the key
   * to leave the proxy unchanged; send `null` to clear it. See
   * internal/adapters/http/handlers/monitor.go CreateMonitorRequest /
   * UpdateMonitorRequest `proxy_id`.
   */
  proxy_id?: number | null;
  /**
   * Manual display order (lower first). Omitted/0 on create becomes 2000.
   * On update, omit the key to leave order unchanged; send 0 to pin to the top.
   */
  weight?: number;
}

export interface UpdateMonitorInput extends Partial<CreateMonitorInput> {}

/**
 * `Monitor` (web/src/lib/stores/ws.svelte.ts) is the WS-fed wire shape and
 * does not yet declare `group_id`/`proxy_id` in its TS type — they're owned
 * by another workstream. Both the REST API (`internal/adapters/http/handlers/monitor.go`
 * `MonitorView`) and the WS wire payloads (`internal/adapters/ws/wire.go`)
 * DO send `group_id` on the wire, so callers that need it (group pickers,
 * tree rendering) should narrow to this local type instead of editing the
 * shared `Monitor` type.
 */
export type MonitorWithGroup = Monitor & {
  group_id?: number | null;
  proxy_id?: number | null;
  owner?: string;
  inherit_group_owner?: boolean;
  /** Resolved contact for display (group chain when inheriting). */
  effective_owner?: string;
};

/**
 * The embedded tag shape carried on every monitor payload (REST + WS). Defined
 * with the `Monitor` wire type it belongs to; re-exported here so callers that
 * already import from this module do not need a second import.
 */
export type { MonitorTagView } from "$lib/stores/ws.svelte.ts";

export const monitorsApi = {
  async list(params?: {
    active?: boolean;
    type?: string;
    search?: string;
  }): Promise<Monitor[]> {
    return api.get<Monitor[]>("/monitors", params);
  },

  async get(id: number): Promise<Monitor> {
    return api.get<Monitor>(`/monitors/${id}`);
  },

  async create(input: CreateMonitorInput): Promise<Monitor> {
    return api.post<Monitor>("/monitors", input);
  },

  async update(id: number, input: UpdateMonitorInput): Promise<Monitor> {
    return api.put<Monitor>(`/monitors/${id}`, input);
  },

  async remove(id: number): Promise<void> {
    return api.del(`/monitors/${id}`);
  },

  async clone(id: number): Promise<Monitor> {
    return api.post<Monitor>(`/monitors/${id}/clone`);
  },

  async pause(id: number): Promise<Monitor> {
    return api.put<Monitor>(`/monitors/${id}`, { active: false });
  },

  async resume(id: number): Promise<Monitor> {
    return api.put<Monitor>(`/monitors/${id}`, { active: true });
  },
};
