import { api } from "./client";

/** All revision counters stay decimal strings, including values above 2^53. */
export interface ProbeView {
  id: string;
  key: string;
  name: string;
  location: string;
  kind: string;
  enabled: boolean;
  enrollment_state: string;
  connection_status: string;
  execution_status: string;
  last_seen_at: string | null;
  agent_version: string | null;
  protocol_version: number | null;
  desired_config_revision: string;
  applied_config_revision: string;
  queue_bytes: number | null;
  oldest_queued_at: string | null;
  revision: string;
  created_at: string;
  updated_at: string;
}
export interface ProbeDetailView extends ProbeView {
  endpoint: string | null;
  tls_fingerprint: string | null;
  certificate_expires_at: string | null;
  credential_version: string | null;
  capabilities: string[];
  diagnostics: {
    enrollment: {
      state: string;
      credential_version: string;
      certificate_version: string;
      certificate_expires_at: string | null;
      prepared_at: string;
      activated_at: string | null;
    } | null;
    connection: {
      owner_id: string;
      generation: string;
      lease_until: string | null;
      connected: boolean;
      live: boolean;
    } | null;
    runtime: {
      owner_id: string;
      epoch: string;
      lease_until: string | null;
      live: boolean;
    } | null;
    watchdog: {
      status: string;
      version: string;
      config_revision: string;
      armed: boolean;
      pending_loss: boolean;
      incident_open: boolean;
      updated_at: string;
    } | null;
    config: {
      desired: { revision: string; effective_at: string } | null;
      applied: {
        revision: string;
        applied_at: string;
        assignment_count: number;
      } | null;
      sync_status: string | null;
    };
  };
}
export interface ProbeOperationView {
  remote_confirmed?: boolean;
  operation_id: string;
  probe_id: string;
  status: string;
  phase: string;
  created_at: string;
  updated_at: string;
  error: { code: string; message: string } | null;
}
export interface ProbeRegistrationInput {
  /** Stable identity printed by the initialized edge agent. */
  probe_id?: string;
  key: string;
  name: string;
  location: string;
  endpoint: string;
  tls_fingerprint: string;
}
/**
 * One-time SSH-password host preparation (POST /api/probe-host-bootstraps).
 * The password and private key are sent once and never stored or returned.
 */
export interface ProbeHostBootstrapInput {
  /** host:port */
  ssh_address: string;
  ssh_user: string;
  /** Login password; also fed to `sudo -S` when ssh_user is not root. */
  ssh_password: string;
  /** Pinned OpenSSH SHA256:… fingerprint of the host key. */
  host_key_fingerprint: string;
  /** Deployment key; its public half becomes the restricted root entry. */
  private_key: string;
  helper_version: string;
  helper_architecture: "amd64" | "arm64";
  helper_sha256: string;
  helper_size: number;
}
export interface ProbeHostBootstrapView {
  kind: "ssh_probe_host_bootstrap";
  status: "verified";
  helper: {
    version: string;
    architecture: string;
    state: "installed" | "unchanged";
  };
  stage_authority: "installed" | "unchanged";
  authorized_key: "added" | "unchanged";
}
export interface ProbePage {
  items: ProbeView[];
  next_cursor: string | null;
}
const probePath = (id: string) => `/probes/${encodeURIComponent(id)}`;
export const probesApi = {
  list(cursor?: string): Promise<ProbePage> {
    return api.get("/probes", { limit: 100, ...(cursor ? { cursor } : {}) });
  },
  detail(id: string): Promise<ProbeDetailView> {
    return api.get(probePath(id));
  },
  create(input: ProbeRegistrationInput): Promise<ProbeDetailView> {
    return api.post("/probes", input);
  },
  update(
    id: string,
    input: {
      name: string;
      location: string;
      enabled: boolean;
      revision: string;
    },
  ): Promise<ProbeDetailView> {
    return api.patch(probePath(id), input);
  },
  enroll(
    id: string,
    enrollment_token: string,
    stream_id?: string,
  ): Promise<ProbeOperationView> {
    return api.post(`${probePath(id)}/enroll`, {
      enrollment_token,
      ...(stream_id ? { stream_id } : {}),
    });
  },
  rotate(id: string, credential_version: string): Promise<ProbeOperationView> {
    return api.post(`${probePath(id)}/rotate-credential`, {
      credential_version,
    });
  },
  reset(
    id: string,
    stream_id: string,
    enrollment_operation_id: string,
  ): Promise<ProbeOperationView> {
    return api.post(`${probePath(id)}/reset-stream`, {
      stream_id,
      enrollment_operation_id,
    });
  },
  revoke(
    id: string,
    reason: string,
  ): Promise<ProbeOperationView & { remote_confirmed: boolean }> {
    return api.post(`${probePath(id)}/revoke`, { reason });
  },
  remove(id: string): Promise<void> {
    return api.del(probePath(id));
  },
  operation(id: string): Promise<ProbeOperationView> {
    return api.get(`/probe-operations/${encodeURIComponent(id)}`);
  },
  bootstrapHost(
    input: ProbeHostBootstrapInput,
  ): Promise<ProbeHostBootstrapView> {
    return api.post("/probe-host-bootstraps", input);
  },
};

/** Error payloads are structured plain objects from ApiClient, not Error instances. */
export function errorCode(error: unknown): string | undefined {
  return typeof error === "object" &&
    error !== null &&
    "code" in error &&
    typeof error.code === "string"
    ? error.code
    : undefined;
}
export function errorMessage(error: unknown, fallback: string): string {
  return typeof error === "object" &&
    error !== null &&
    "message" in error &&
    typeof error.message === "string"
    ? error.message
    : fallback;
}
