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
  /**
   * Create-only launch flag. The form always sends true (new monitors run);
   * omitted on the wire the backend also defaults to active. There is no
   * deferred-activation flow any more — the initial assignment set rides the
   * same atomic create request (issue #60).
   */
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
  /**
   * Initial desired execution sources for the atomic create path (protocol
   * section 7.1 — `CreateMonitorRequest` in internal/adapters/http/handlers/
   * monitor.go). Omission means ["local"] + any_down (the local-default path);
   * explicit remote membership requires admin. Create + the explicit set
   * commit in one transaction, so a rejected set leaves no monitor behind.
   * Create-only: PUT /api/monitors/:id has no assignment fields (use
   * PUT /api/monitors/:id/probes for later changes).
   */
  probe_ids?: string[];
  /** Initial overall health policy (`health_policy`): "any_down" | "all_down". */
  health_policy?: string;
  /**
   * Initial per-probe local resource bindings (docker remote targets), as
   * `probe_bindings` — each entry binds one probe to one probe-local resource.
   */
  probe_bindings?: MonitorProbeBindingInput[];
}

/** One `probe_bindings` entry — matches `MonitorProbeBindingRequest` json tags. */
export interface MonitorProbeBindingInput {
  probe_id: string;
  kind: string;
  binding_key: string;
}

/**
 * The assignment editor's draft (`ProbeAssignments.initialAssignments()`),
 * submitted with the create request so the backend commits monitor + desired
 * set atomically (issue #60). Mirrors `services.InitialAssignments` on the
 * backend. `probe_bindings` carries only the bindings of SELECTED probes.
 */
export interface MonitorInitialAssignments {
  probe_ids: string[];
  health_policy: string;
  probe_bindings?: MonitorProbeBindingInput[];
}

/**
 * Form state flattened for body building (MonitorForm.handleSubmit values).
 * Pre-normalized fields (config) arrive ready to send; raw fields (the
 * accepted-status-code list) are transformed by the builders.
 */
export interface MonitorDraftValues {
  name: string;
  description: string;
  owner: string;
  inheritGroupOwner: boolean;
  type: string;
  interval: number;
  timeout: number;
  retryInterval: number;
  maxRetries: number;
  resendInterval: number;
  weight: number;
  upsideDown: boolean;
  tlsIgnore: boolean;
  certExpiryNotify: boolean;
  /** Comma-separated as edited; split into `accepted_statuscodes` on save. */
  acceptedStatusCodes: string;
  /** Already normalized for submission (headers parsed, JSON assertion folded). */
  config: Record<string, unknown>;
  /** `group_id` as picked: null sends JSON null (top-level / clear). */
  groupId: number | null;
  /** `proxy_id` as picked: null sends JSON null (no proxy / clear). */
  proxyId: number | null;
}

export interface UpdateMonitorInput extends Partial<
  Omit<CreateMonitorInput, "probe_ids" | "health_policy" | "probe_bindings">
> {}

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

/** Shared field set of the create/update bodies (backend ignores `type` on PUT). */
function baseBody(v: MonitorDraftValues) {
  return {
    name: v.name,
    description: v.description,
    owner: v.owner,
    inherit_group_owner: v.inheritGroupOwner && v.groupId !== null,
    type: v.type,
    interval: v.interval,
    timeout: v.timeout,
    retry_interval: v.retryInterval,
    max_retries: v.maxRetries,
    resend_interval: v.resendInterval,
    // Send the intended weight so a reorder persists (0 is a real value).
    weight: v.weight,
    upside_down: v.upsideDown,
    // TLS skip is meaningful for HTTP and S3; never carry a stale toggle onto
    // another type when the user switches type before saving.
    tls_ignore: v.type === "http" || v.type === "s3" ? v.tlsIgnore : false,
    cert_expiry_notify: v.type === "http" ? v.certExpiryNotify : false,
    config: v.config,
    accepted_statuscodes: v.acceptedStatusCodes
      .split(",")
      .map((s) => s.trim())
      .filter(Boolean),
    group_id: v.groupId,
    proxy_id: v.proxyId,
  };
}

/** The backend local-default set (`domain.LocalProbeID` + any_down, no bindings). */
function isDefaultInitialAssignments(
  initial: MonitorInitialAssignments,
): boolean {
  return (
    initial.probe_ids.length === 1 &&
    initial.probe_ids[0] === "local" &&
    initial.health_policy === "any_down" &&
    !initial.probe_bindings?.length
  );
}

/**
 * Build the POST /api/monitors body — always exactly one active create.
 *
 * When `initial` carries a non-default desired set it rides the same request
 * (`probe_ids` / `health_policy` / `probe_bindings`), so the backend commits
 * monitor + set atomically (CreateWithAssignments) — a rejected set fails the
 * create and leaves no monitor behind. A default set, or no editor draft at
 * all (`null`/undefined), is omitted and takes the backend local-default path
 * unchanged. There is no deferred-activation option any more: the draft
 * removes the need for a post-create assignment save, so nothing is paused
 * and no follow-up PUT/resume/delete compensation exists (issue #60).
 */
export function buildMonitorCreateBody(
  values: MonitorDraftValues,
  initial?: MonitorInitialAssignments | null,
): CreateMonitorInput {
  const carriesSet = !!initial && !isDefaultInitialAssignments(initial);
  return {
    ...baseBody(values),
    active: true,
    ...(carriesSet
      ? {
          probe_ids: initial.probe_ids,
          health_policy: initial.health_policy,
          ...(initial.probe_bindings?.length
            ? { probe_bindings: initial.probe_bindings }
            : {}),
        }
      : {}),
  };
}

/**
 * Build the PUT /api/monitors/:id body for an ordinary edit.
 *
 * `active` is deliberately absent: saving name/description/interval changes
 * must preserve the monitor's pause state (issue #58). Omitting it also avoids
 * overwriting a concurrent pause/resume — resuming is the explicit Resume
 * action (monitorsApi.resume), never a side effect of an edit. Initial
 * assignments are create-only and never appear here.
 */
export function buildMonitorUpdateBody(
  values: MonitorDraftValues,
): UpdateMonitorInput {
  return baseBody(values);
}

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
