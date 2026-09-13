# Multi-region probes — protocol and API contract

Status: proposed V1 contract with a bounded executable foundation. See [implementation status](IMPLEMENTATION_STATUS.md) for implemented decoders and unresolved schemas; the complete V1 protocol is not yet frozen. This file specifies new behavior; none of these probe endpoints or messages is claimed to exist in the baseline. Read [ARCHITECTURE.md](ARCHITECTURE.md) for ownership and persistence guarantees.

## 1. General encoding rules

- Protocol name: `phoenix.probe.v1`; WebSocket subprotocol negotiation must select that exact value.
- Runtime endpoint on the probe: `GET /ws/probe/v1`. Hub dials `wss://<configured-host>:<configured-port>/ws/probe/v1` with `Authorization: Bearer <runtime-credential>`. No query-string token.
- JSON objects use explicit transport DTOs with `json:"snake_case"` tags. Domain types are never marshalled directly.
- Timestamps are RFC3339 UTC with `Z`; accept fractional seconds on the wire, retaining the existing database partition-time precision. Durations ending `_ms` are milliseconds; `_seconds` are seconds; existing `resend_interval` remains minutes.
- Probe/hub/stream/command IDs are bounded strings. IDs allocated as UUIDs use their standard lowercase representation; the nil UUID is invalid for frame, stream, and registration identities. Probe `key` is a unique operator-managed slug, distinct from immutable ID; `local` is reserved.
- `tls_fingerprint` is the lowercase 64-character hexadecimal SHA-256 digest of the leaf certificate's DER bytes. The API accepts exactly 64 hexadecimal characters and normalizes case; CLI/display labels may show a separate `SHA256` label but never include colons or that prefix inside the stored value. Compare decoded digest bytes in constant time. Tokens use at least 32 cryptographically random bytes encoded as unpadded base64url: `phx_probe_enroll_` for enrollment and `phx_probe_` for runtime credentials.
- Sequence, revision, and connection-generation values are canonical decimal strings (no leading zeros except the value `"0"`, no sign, no whitespace) representing nonnegative signed 64-bit integers, maximum `9223372036854775807`. Zero is reserved for initial cursors/unnegotiated generations; durable events begin at one. Parse without floating-point conversion. This matches SQLite INTEGER and MariaDB BIGINT storage. Monitor and existing hub entity IDs retain the application's current JSON integer convention.
- `null`, absence, and empty arrays have distinct semantics below. Omitted optional fields retain existing values on PATCH-like updates; full replacements explicitly list every member.
- Unknown optional JSON fields are ignored within a supported major version. Missing required fields, unsupported required capability names, duplicate object keys, overflow, invalid enums, and unknown required message types are rejected. Bound field lengths before persistence.
- Maximum frame: 1 MiB decoded JSON. Maximum telemetry batch: 256 events and 512 KiB for the complete envelope, whichever occurs first. A single event is at most 64 KiB. Large config/state snapshots use chunks.
- Disable compression initially to simplify resource accounting. Enforce decoded limits if enabling compression later.
- Default operation deadlines: handshake 10 seconds, control write 10 seconds, ordinary batch commit 10 seconds. Slow storage returns retryable busy/degraded status without advancing the cursor.

## 2. Session lifecycle

1. Hub verifies endpoint policy, acquires the per-probe DB lease, and loads the required pin and credential.
2. TLS handshake verifies the nonempty expected fingerprint, certificate validity, and TLS 1.3. Probe validates the credential hash in constant time before accepting the WebSocket upgrade.
3. Probe sends `hello`; hub replies `welcome`. Compare installation identity, stream epoch, version, generation, and capabilities. No configuration or telemetry is accepted before this succeeds.
4. Hub begins the latest required configuration snapshot. If the revision/hash already match durable active config, the probe replies `config.applied` immediately and the hub skips chunks; otherwise the complete transfer proceeds. Edge reports durable config activation or a structured rejection.
5. Probe sends a complete current-state snapshot and starts ordered replay from the hub's committed cursor. Control/config/command traffic continues during replay.
6. Application health messages every 15 seconds and transport pings maintain connection diagnostics. Any close cancels all session tasks; durable work stays on disk for reconnection.

### 2.1 Common envelope

Every application message has `protocol_version`, `type`, `message_id`, `sent_at`, `connection_generation`, and `payload`. `hello` uses `connection_generation: "0"`; `welcome` supplies the accepted generation. All later messages must match it. A transport `message_id` is a UUID for correlation, not the durable event deduplication key.

```json
{
  "protocol_version": 1,
  "type": "hello",
  "message_id": "1c73a9c5-77ad-4dc0-926d-123bbab94540",
  "sent_at": "2026-09-13T09:00:00Z",
  "connection_generation": "0",
  "payload": {
    "hub_id": "b1123604-32c5-40aa-89f9-b62f93dceac2",
    "probe_id": "e645246b-b176-4422-8ae5-b79629ee6a29",
    "stream_id": "1395c134-da65-4d86-9b11-c9ce20c61748",
    "agent_version": "0.5.0-dev",
    "protocol_min": 1,
    "protocol_max": 1,
    "config_revision": "12",
    "first_retained_seq": "101",
    "last_created_seq": "148",
    "capabilities": ["checker.http.v1", "checker.tcp.v1", "checker.dns.v1", "notifier.telegram.v1", "snapshot.v1"],
    "resource_bindings": []
  }
}
```

The example version is illustrative, not a release-number reservation. The actual registry advertises only tested capabilities available in that build/runtime.

`hello.resource_bindings` advertises only `binding_key` and `kind` (`docker_socket` or `docker_api`) from the operator's local resource-binding file. It contains no host path, secret, or arbitrary execution instruction. An admin maps a monitor's Docker requirement onto one advertised key in its probe assignment. The probe resolves that key into its locally configured socket/API resource; mismatched kind or unknown key rejects config activation. A probe with no Docker binding cannot accept a Docker assignment even if the checker code is compiled in.

`welcome.payload` has: `hub_id`, `probe_id`, `stream_id`, `selected_protocol` (integer), `connection_generation`, `committed_seq`, `desired_config_revision`, `heartbeat_seconds` (15), `max_frame_bytes` (1048576), `max_batch_events` (256), `max_batch_bytes` (524288), and `hub_time`. A stream unknown to the hub is rejected with `stream_reset_required`; a reset is an authenticated administrative action, not a client request to discard history.

### 2.2 Application health

`health.payload` has `role` (`hub` or `probe`), `ready` (boolean), `db_writable` (boolean), `scheduler_healthy` (boolean or null for a hub API role), `config_revision`, `committed_seq`, `queue_bytes`, `oldest_queued_at` (timestamp or null), `clock_time`, and `errors` (bounded array of redacted error codes). Hub health describes ingest readiness; probe health describes local execution readiness. Values irrelevant to the sender are null, never fabricated zeros.

Estimate clock offset from bounded request/response timing, not a one-way arrival timestamp. Default skew diagnostic threshold is 30 seconds. Use monotonic elapsed time for watchdogs. Skewed evidence does not become fresh simply because a frame arrived recently.

## 3. Configuration snapshots

### 3.1 Transfer and activation

Use `config.begin`, `config.chunk`, `config.commit`, `config.applied`, and `config.rejected`:

| Type | Required payload fields |
|---|---|
| `config.begin` | `snapshot_id`, `revision`, `config_schema_version` (1), `total_bytes`, `chunk_count`, `sha256`, `required_capabilities`, `effective_at` |
| `config.chunk` | `snapshot_id`, `revision`, `index` (zero-based integer), `data_base64` |
| `config.commit` | `snapshot_id`, `revision`, `sha256` |
| `config.applied` | `snapshot_id`, `revision`, `sha256`, `applied_at`, `assignment_count` |
| `config.rejected` | `snapshot_id`, `revision`, `errors` containing `path`, `code`, and redacted `message` |

The hub serializes the complete snapshot once and hashes those exact UTF-8 bytes. The probe reconstructs and hashes the same bytes; it does not reserialize JSON to verify the hash. Default total snapshot limit is 16 MiB, chunk payload limit 256 KiB before base64 encoding, and staging timeout 60 seconds. Reject excess before allocation. Repeated identical chunks are idempotent; conflicting duplicate chunks abort staging. An interrupted snapshot never replaces the active version.

`config_revision` is monotonic per probe. Replaying the currently applied revision with the same hash returns `config.applied`; the same revision with a different hash returns `revision_conflict`. Lower revisions cannot reactivate old assignments. A deliberate rollback is a new higher revision carrying the earlier desired content.

### 3.2 Complete snapshot schema

The top-level object has all of these members:

| Field | Type and meaning |
|---|---|
| `schema_version` | Integer 1 |
| `hub_id`, `probe_id` | Bound authority and destination |
| `revision` | Decimal string |
| `created_at`, `effective_at` | UTC timestamps |
| `assignments` | Array of the assignment objects below; empty is valid for an unassigned probe |
| `notification_channels` | Array of channel objects below |
| `notification_templates` | Array of resolved template objects: `id`, `provider`, `name`, `version`, `title_template`, `body_template`, `config`; `config` is the explicit provider template config used by the existing renderer |
| `maintenance_windows` | Array of maintenance objects below |
| `proxy_bindings` | Array of `binding_key`, `version`, `protocol`, `host`, `port`, `auth`, `username`, `password`, `active` |
| `escalation_policies` | Array of resolved policies: `id`, `version`, `enabled`, `steps`; each step has `step`, `delay_seconds`, `notification_ids`. Delay is after the previous step, translated from existing `WaitMinutes` by multiplying by 60; step zero stays dispatcher-owned |
| `watchdog` | `enabled`, `lost_after_seconds`, `recover_after_seconds`, `notification_ids`, `resend_interval` |

An assignment contains `monitor_id`, `generation`, `active`, `monitor`, `notification_ids`, `notification_links` (objects with `notification_id` and `include_target`), `maintenance_ids`, `proxy_binding_key` (string or null), `resource_bindings` (objects with `kind` and `binding_key`), `escalation_policy_id` (integer or null), and `required_capabilities`. Arrays are complete replacements. References must resolve within the same snapshot or to a declared local runtime binding; unresolved references reject the whole activation. Escalation selection preserves direct-monitor then nearest-ancestor precedence, including disabled-policy suppression of inheritance.

`monitor` contains an explicit whitelist of execution/template fields verified against the existing monitor model and handlers:

`name`, `description`, `owner`, `effective_owner`, `type`, `interval`, `retry_interval`, `max_retries`, `timeout`, `config`, `accepted_statuscodes`, `upside_down`, `tls_ignore`, `cert_expiry_notify`, `resend_interval`, and `tags` (objects with `name`, `value`). Keep `accepted_statuscodes` exactly as the existing HTTP JSON tag. Do not substitute `accepted_status_codes`. `timeout` is seconds, including fractions. Raw checker PENDING is valid: gRPC UNKNOWN/SERVICE_UNKNOWN responses already produce it. The existing type-specific `config` is validated by the checker adapter; export only required fields and secrets. No user records or administrative permissions travel with it.

Dependency `version` fields on channels, templates, proxies, and escalation policies use the containing positive snapshot revision as a canonical decimal string in V1. They are not derived from timestamps. A rebuilt snapshot gives all dependencies its revision; durable old provider intents must reconcile against the current complete snapshot before sending. The unique `notification_ids` set must exactly equal the IDs in `notification_links`. Direct delivery follows per-link target visibility; escalation preserves the existing default include-target behavior and must carry its separate delivery context.

A notification channel contains `id`, `version`, `type`, `name`, `active`, `config`, `template_id` (integer or null), and `include_ack_url`. For remote V1 snapshots, `include_ack_url` must be false: the builder explicitly resolves the channel preference to unsupported, and activation rejects true. Existing local opaque-token acknowledgement URLs are unchanged. Remote acknowledgements use authenticated hub commands after incident mirroring. Target inclusion is per assignment/channel link through `notification_links`, because the same channel can have different settings on different monitors. This is a new resolved transport DTO, not a raw serialization of the notification domain. Provider configuration keys retain the corresponding sender's `Validate` contract. Group notification attachments are not inherited by individual monitors; their incidents remain hub-owned. Channels referenced by effective escalation steps and the watchdog are also included even when absent from direct monitor notification IDs.

A maintenance object contains `id`, `active`, `strategy`, `start_date`, `end_date`, `cron_expr`, `duration`, `timezone`, and `monitor_ids`. Times may be null when the strategy does not use them. `duration` is minutes for cron windows, verified in `internal/core/domain/maintenance.go` and the maintenance service. Empty `monitor_ids` is not used to ambiguously signal both none and all: the hub expands effective applicability into assignment `maintenance_ids`.

The snapshot is confidential and must not be logged. Resolve secrets server-side using the dedicated protected store. Ordinary API responses return only references and readiness flags.

## 4. Telemetry and idempotent ingest

### 4.1 Durable event schema

A durable event has `seq`, `kind`, `observed_at`, and `data`. It is identified by the authenticated `(probe_id, stream_id, seq)`. Supported V1 kinds are `observation`, `alert.transition`, `delivery.result`, `condition.transition`, and `watchdog.transition`. Each event is immutable once assigned a sequence.

```json
{
  "protocol_version": 1,
  "type": "telemetry.batch",
  "message_id": "637c39e5-0d90-41db-82c8-4a8bc65f16e5",
  "sent_at": "2026-09-13T09:00:05Z",
  "connection_generation": "7",
  "payload": {
    "stream_id": "1395c134-da65-4d86-9b11-c9ce20c61748",
    "first_seq": "101",
    "last_seq": "101",
    "events": [
      {
        "seq": "101",
        "kind": "observation",
        "observed_at": "2026-09-13T08:59:58Z",
        "data": {
          "monitor_id": 42,
          "assignment_generation": "3",
          "config_revision": "12",
          "status": "UP",
          "raw_status": "UP",
          "down_count": 0,
          "ping": 28,
          "duration_ms": 31,
          "message": "HTTP 200",
          "important": false,
          "conditions": [],
          "tls": null
        }
      }
    ]
  }
}
```

Observation `status` is one of `UP`, `DOWN`, `PENDING`, `MAINTENANCE`; UNKNOWN is a hub projection or execution diagnostic rather than a fabricated successful checker observation. `raw_status` is the checker result before retry confirmation and allows `UP`, `DOWN`, `PENDING`, or `MAINTENANCE`; raw PENDING remains PENDING with a zero failure count; a maintenance observation uses `MAINTENANCE` for both. Validate nonnegative counts/latencies, configured identity, allowed status transitions, and time bounds; the source still owns evaluation. Bound redacted `message` to 4096 UTF-8 bytes. Preserve boolean `important` as source transition metadata.

Observation `conditions` are raw checker evidence; they do not establish a promoted current condition. Each `conditions` item has `kind` (`session_pool` or `storage`), `state` (`ok`, `warning`, `error`), `message`, `used`, `limit`, `percent`, `threshold` (each number or null), `unit`, `resource`, `scope`, `source` (strings), `observed_at`, and `stale_after`. The source transport builder must fill `observed_at` and derive `stale_after` from the accepted monitor configuration before serialization; raw checker observations may leave freshness unset. These fields map the verified `ConditionObservation` type in `internal/core/domain/monitor_condition.go`. `stale` is derived from freshness and is not an observed stored state. `tls` is null or a sanitized object with `not_after`, `days_remaining`, and `issuer`. Map these transport objects explicitly into regional condition/certificate stores; no credentials, complete certificates, or raw checker metadata map is accepted blindly.

`alert.transition` and `watchdog.transition` data contain `source_alert_id`, `scope`, `monitor_id` (nullable only for probe watchdogs), `assignment_generation` (nullable for watchdogs), `status` (`firing`, `acked`, `resolved`), `transition_version`, `started_at`, `resolved_at` (nullable), `reason`, and `config_revision`. Use a monotonic incident transition version to reject stale mirrored updates. Source alert IDs are persisted lowercase UUIDs, never hub numeric row IDs. Before these event DTOs are implemented, freeze subject discriminators for availability/capacity/certificate/watchdog, condition kind and certificate threshold/expiry, acknowledgement actor/time, and escalation progress; those fields remain an explicit M0 blocker. The observation-only decoder does not accept these events yet.

`delivery.result` data contain `delivery_id`, `source_alert_id`, `notification_id`, `notification_version`, `event_kind`, `attempt`, `status` (`sent`, `retrying`, `failed`, `superseded`), and `error_code` (nullable, redacted). It describes the source provider attempt; the hub never treats it as a send request.

`condition.transition` data contain `monitor_id`, `assignment_generation`, `config_revision`, `kind`, `state`, `message`, and `source_alert_id` (nullable). Preserve the existing capacity promotion and availability separation rules.

### 4.2 Commit and ACK rules

V1 uses one in-flight telemetry batch per session. The next batch is sent after acknowledgement or retried after timeout. This deliberately bounds ordering complexity; optimize concurrency only with a new durable receipt contract.

`telemetry.ack.payload` contains `stream_id`, `committed_seq`, `accepted_count`, `duplicate_count`, and `rejected` (array of permanently rejected `seq`, `code`). The cursor represents every preceding sequence either durably accepted, durably rejected with a receipt/reason, or covered by an explicit persisted gap.

The hub transaction locks the stream cursor, ignores duplicate prefixes, authorizes each new event, inserts accepted history/mirrors, records permanent rejections, marks dirty buckets, and advances the contiguous cursor. A retryable storage failure rolls back the whole new suffix and produces `telemetry.retry` with required `stream_id`, `committed_seq` (unchanged canonical decimal string), and `retry_after_ms` (positive signed-64-bit JSON integer). Authentication failure closes the session. Malformed envelope/sequence framing rejects the batch without skipping it; the probe surfaces a blocked queue rather than guessing.

Permanent per-event failures such as a deleted assignment can be acknowledged only after recording the rejection outcome. They must not block all later valid events forever. Store rejection/gap receipts outside partitioned heartbeats, bounded by the retention/stream retirement policy. Preserve enough receipt metadata for operator diagnostics and cursor recovery.

Do not advance the cursor before transaction commit. Do not acknowledge an event merely because an in-memory queue accepted it. EventBus/browser fan-out happens after commit and may be retried or resynchronized independently.

### 4.3 Gaps and retention

`telemetry.gap.payload` contains `stream_id`, `from_seq`, `through_seq`, `reason` (`retention_bytes`, `retention_age`, `disk_pressure`, `restore_loss`, `history_cleared`), `observed_from`, `observed_through`, and `affected_monitor_ids` (bounded array; empty means unknown coverage, not no affected monitors). It must begin at or overlap the hub's next expected sequence. Hub persists the gap, marks affected coverage/aggregates dirty, advances only the contiguous covered interval, and replies with `telemetry.ack`.

Ordinary sender retention gaps cannot erase committed rows. History clearing is a separate authorized hub command that installs a watermark and uses receipts to discard later replay of cleared evidence. Stream retirement is explicit; do not delete active cursor state while queued retries could arrive.

## 5. Current-state snapshot

Use `state.begin`, `state.chunk`, `state.commit`, and `state.applied` with the same chunk/hash discipline as config snapshots. State transfer is allowed while telemetry backlog exists. The complete snapshot contains `stream_id`, `config_revision`, `created_at`, `last_created_seq`, and `states`.

Unlike raw observation conditions, each current-state `conditions` entry must carry `kind`, `observed_state`, nullable `effective_state` (null means not yet confirmed), `consecutive_state`, `consecutive_count`, `last_success_at` (nullable), and the raw numeric/diagnostic/timestamp fields. Valid effective/candidate states are `ok`, `warning`, and `error`; stale is derived. Hub ingest mirrors the effective state without rerunning promotion. Fixtures must cover a first-ever warning (effective null), a first warning after OK (effective OK), second-sample warning, first/second recovery, and staleness. Notification cursors remain source-owned and are not inferred from raw evidence. Exact chunk and full state DTOs remain pending M0 work.

Each state has `monitor_id`, `assignment_generation`, `last_observation_seq`, `observed_at`, `status`, `down_count`, `ping`, `message`, `conditions`, `tls`, and `active_source_alert_id` (nullable). Omitted active assignments become UNKNOWN with `missing_snapshot_state`; they never retain fresh evidence indefinitely. An empty snapshot is valid for a probe with no assignments.

The hub validates assignment and config generations and atomically applies entries newer than their current per-assignment sequence. A state snapshot does not advance the historical telemetry cursor, create a raw heartbeat, infer missing transitions, or trigger regional provider delivery. Fresh snapshots can drive overall displayed health. Optional aggregate paging must use its own current-evidence reconciliation rules.

## 6. Commands, enrollment, and rotation

### 6.1 Command contract

`command.request.payload` has `command_id`, `kind`, `created_at`, `expires_at`, `target`, and `data`. The target identifies probe, assignment generation when applicable, and source incident ID when applicable. Allowed V1 kinds are `alert.ack`, `probe.stop`, `history.clear`, `credential.prepare`, `credential.activate`, `certificate.prepare`, and `certificate.activate`. Configuration changes use snapshots, not an unbounded generic command API.

`command.result.payload` has `command_id`, `status` (`applied`, `already_applied`, `already_resolved`, `rejected`, `expired`), `applied_at` (nullable), `code` (nullable), and redacted `message`. The probe persists result identity before acknowledging. Retain command IDs through at least the command expiry plus the maximum supported reconnect/retention window. A repeated command returns its recorded result.

`alert.ack.data` is `source_alert_id`, `actor_display_name`, and `note` (bounded, optional). It affects only that incident. `probe.stop` durably disables scheduling after accepted shutdown instructions. `history.clear` contains an explicit observation-time/sequence watermark and scope. Only defined fields are accepted; there is no shell execution command.

| Command kind | Exact data members |
|---|---|
| `alert.ack` | `source_alert_id`, `actor_display_name`, optional `note` |
| `probe.stop` | `reason`, `effective_at`; only the bound hub/admin authority may issue it |
| `history.clear` | `monitor_id`, `assignment_generation`, `through_observed_at`, `through_seq`, `clear_id`; both bounds constrain discarded evidence |
| `credential.prepare` | `rotation_id`, `credential_version`, `token`, `overlap_expires_at`; token is confidential and write-only |
| `credential.activate` | `rotation_id`, `credential_version` |
| `certificate.prepare` | `rotation_id`, `certificate_version`, `valid_for_days`; probe generates/persists the pending key/certificate locally |
| `certificate.activate` | `rotation_id`, `certificate_version`, `expected_fingerprint` |

`command.result` may additionally include a `details` object for certificate preparation containing `certificate_version`, `tls_fingerprint`, and `not_after`, or credential preparation containing only `credential_version`. No result includes a token or private key. A config snapshot with active assignments is the explicit way to resume scheduling after an administrative stop; a lost connection cannot undo a persisted pause.

### 6.2 Manual enrollment

1. Probe initialization writes stable identity and a self-signed TLS certificate to its protected data directory, then outputs the endpoint suggestion, certificate SHA-256 pin, and a 10-minute single-use enrollment token to the local operator. Output is shown once; server logs omit it.
2. An admin obtains those values through the VM console or verified SSH, creates a hub probe registration, and submits the token through the write-only enrollment endpoint.
3. The hub connects to `/ws/probe/enroll/v1` using the pin and enrollment token, presents its `hub_id` and assigned `probe_id`, and negotiates protocol/capabilities. The probe refuses binding to a different existing hub without explicit local reset.
4. Before sending a new runtime credential, the hub durably stores it encrypted with a stable `enrollment_id`. Probe durably records the runtime token hash and binding before consuming the enrollment token and responding.
5. Hub reconnects to the runtime endpoint with the persisted runtime token. If the enrollment response was lost, this reconnect recovers the completed exchange. If preparation failed before probe commit, the same unexpired enrollment ID/token may be retried. Expired uncommitted enrollment requires a fresh local token.
6. Hub marks enrollment active only after a runtime handshake succeeds. Monitor execution begins after the first valid config activation. No success is inferred from an SSH process exit code alone.

### 6.3 Credential/certificate rotation

Prepare uses a stable rotation ID and a version greater than active. The hub persists the new token or expected certificate pin first; the probe persists its pending token hash or certificate/key before reporting prepared. Activation is a separate idempotent command. Permit both old and prepared credentials/pins only during the explicit 10-minute overlap and then retire the old version. A lost activation response is recovered using the new identity, never by dropping validation.

Certificate preparation transfers/advertises the new certificate fingerprint through the currently authenticated pinned session; private keys remain on the probe. Refuse expired certificates. Reenrollment after full credential loss is an operator action with verified host identity.

## 7. Hub administrative HTTP API

These are proposed endpoints under the existing Echo router. Feature-disabled routes return a typed unavailable/not-implemented response; they do not return fake success. Use explicit Views and the existing auth/error helpers.

Fleet routes require an authenticated admin; follow the established session-or-write-API-key pattern for programmatic administration. Regional monitor reads use the current monitor visibility policy through AccessService. Authenticated users without visibility receive 404. Fleet secrets are never present in read responses.

| Method and path | Request / result |
|---|---|
| `GET /api/probes` | Admin fleet list with pagination; returns `items`, `next_cursor` |
| `POST /api/probes` | Create registration using `key`, `name`, `location`, `endpoint`, `tls_fingerprint`; returns 201 ProbeView, initially `unconfigured` |
| `GET /api/probes/:probe_id` | Admin ProbeView |
| `PATCH /api/probes/:probe_id` | Update `name`, `location`, `enabled` with expected `revision`; endpoint/pin changes require explicit identity workflow |
| `POST /api/probes/:probe_id/enroll` | Write-only `enrollment_token`; returns 202 operation receipt |
| `POST /api/probes/:probe_id/rotate-credential` | Expected credential version; returns 202 operation receipt; no plaintext token in response |
| `POST /api/probes/:probe_id/revoke` | `reason`; returns revocation receipt with `remote_confirmed` boolean |
| `POST /api/probes/:probe_id/reset-stream` | New locally verified stream identity plus reenrollment operation reference; returns 202 receipt |
| `DELETE /api/probes/:probe_id` | 409 while actively assigned; otherwise soft-delete, revoke and retain historical attribution; `local` cannot be deleted |
| `GET /api/probe-operations/:operation_id` | Admin operation state, phase and redacted errors |
| `GET /api/monitors/:id/probes` | Authorized regional assignment summaries; no endpoint/secrets/fleet totals |
| `PUT /api/monitors/:id/probes` | Admin atomic complete assignment/policy replacement, described below |
| `GET /api/monitors/:id/probes/:probe_id/heartbeats` | Authorized regional history, preserving existing `hours`, `limit`, `order`, `important` query semantics |
| `GET /api/monitors/:id/probes/:probe_id/heartbeats/chart` | Regional chart; validates monitor/probe relationship before access |
| `GET /api/monitors/:id/health` | Overall health, regional counts, coverage, policy and projection version |
| `POST /api/monitors/:id/probe-alerts/:alert_id/ack` | Existing authenticated monitor-visibility acknowledgement authority (no new capability flag); returns 202 command receipt pending remote application |

`ProbeView` fields: `id`, `key`, `name`, `location`, `kind`, `enabled`, `enrollment_state`, `connection_status`, `execution_status`, `last_seen_at`, `agent_version`, `protocol_version`, `desired_config_revision`, `applied_config_revision`, `queue_bytes`, `oldest_queued_at`, `revision`, `created_at`, `updated_at`. Admin detail additionally exposes `endpoint`, `tls_fingerprint`, `certificate_expires_at`, `credential_version`, and `capabilities`; none of those grant runtime authentication. Unknown values are null.

Connection states are `never_connected`, `online`, `suspect`, `disconnected`, `revoked`; enrollment states are `unconfigured`, `pending`, `active`, `failed`; execution states are `unconfigured`, `ready`, `degraded`, `paused`, `revoked`. Keep these independent of target availability status.

An operation receipt has `operation_id`, `probe_id`, `status` (`pending`, `running`, `succeeded`, `failed`), `phase`, `created_at`, `updated_at`, and `error` (nullable object containing `code`, `message`). A command receipt has `command_id`, `status` (`pending`, `applied`, `failed`, `expired`), and `remote_confirmed`.

### 7.1 Assignment replacement

```json
{
  "expected_revision": "4",
  "probe_ids": ["local", "e645246b-b176-4422-8ae5-b79629ee6a29"],
  "health_policy": "any_down",
  "alert_delivery": "regional"
}
```

Reject duplicate IDs, unknown/revoked probes, empty lists, unavailable capabilities, incompatible required proxy/Docker bindings, and remote push assignments. `expected_revision` is mandatory for replacement; conflicts return 409. Validate all members before committing any change. The response is 200 with `revision`, `health_policy`, `alert_delivery`, and `assignments` containing `probe_id`, `generation`, `desired_config_revision`, `applied_config_revision`, and `sync_status` (`pending`, `applied`, `rejected`). Saving desired state succeeds independently of remote connectivity, and the UI shows pending application.

Assignment replacement additionally accepts optional `bindings`, an array of `probe_id`, `kind`, and `binding_key` objects for selected probes. Omission preserves bindings on retained assignments and supplies none for new assignments. An explicit empty array clears bindings; validate that every resulting assignment still has its required resources before commit. The response includes resolved binding keys, never underlying socket paths or API credentials. Network proxy settings use existing monitor proxy configuration materialized into `proxy_bindings`; they do not require a new arbitrary local-resource kind.

The monitor create request may add optional `probe_ids`, `health_policy`, and `probe_bindings` (same members as assignment `bindings`) for admin callers. Omission means `["local"]` and `any_down`; creation plus requested assignments commits atomically. A non-admin creator retains today's local behavior and receives 403 if explicitly attempting remote assignment. Monitor updates preserve assignments when those fields are absent; assignment replacement is preferably routed through the dedicated revisioned endpoint. Clone of a remote monitor by a non-admin is rejected rather than silently rerouted.

### 7.2 Read shapes and compatibility

Existing HTTP heartbeat status is lowercase; probe observation status is uppercase. Legacy browser heartbeats use `msg` and map maintenance to `paused` and unknown integers to `pending`. Keep separate DTOs and fixtures. Before emitting UNKNOWN on existing consumers, deliberately update those mappings and all frontend readers; merely adding the domain constant does not activate it. New regional HTTP/browser status uses lowercase `unknown`.

Existing HTTP heartbeat wire names remain `id`, `monitor_id`, `status`, `ping`, `message`, `time`, `important`. The regional endpoint adds `probe_id`, `received_at`, `assignment_generation`, and `config_revision`; it does not rename `message` to the domain field `Msg`.

The existing unqualified monitor heartbeat endpoint represents overall monitor history for a multi-probe monitor. Add `scope: "overall"` and `latency_available: false`; `ping` is the existing unmeasured zero sentinel. Overall chart responses have no synthetic latency buckets and carry downtime/unknown intervals. The updated UI uses selected regional endpoints for latency. A local-only monitor preserves today's measured response behavior. This is an explicit compatibility change to verify against every dashboard, badge, status-page, and external client contract before activation.

Overall HealthView fields: `monitor_id`, `status`, `health_policy`, `projection_version`, `as_of`, `uptime_percent` (number or null), `coverage_percent` (number or null), `known_seconds`, `unknown_seconds`, `maintenance_seconds`, `probe_counts` (assigned/up/down/pending/unknown/maintenance/paused), and `regions`. A region contains only `probe_id`, `name`, `location`, `status`, `connection_status`, `observed_at`, `received_at`, `fresh_until`, `config_sync_status`, and `reason` (nullable). Public status views omit `regions` in V1.

### 7.3 Error semantics

Preserve the existing error key: `{ "error": "human-readable explanation", "code": "machine_code" }`. The additive machine-readable `code` does not rename `error` to `message`. New validation endpoints may add `fields`, mapping paths to error codes. The important statuses are 400 malformed input; 401 missing/invalid credential; 403 authenticated but missing administrative authority; 404 hidden or absent monitor; 409 revision/identity/assignment conflict; 413 size limit; 422 supported request with invalid configuration; 429 rate limit; 503 transient unavailable storage/feature activation; 501 endpoint deliberately not implemented in a staged build. Never report 200/204 for an unperformed mutation.

## 8. Browser WebSocket and caching

The browser WebSocket is not the probe transport. Continue to use `internal/adapters/ws/events.go` and explicit wire mappers, paired with `web/src/lib/stores/ws.svelte.ts`.

| Event | Audience and payload |
|---|---|
| Existing `heartbeat` / `status.change` / `stats.update` | Overall monitor-compatible payloads; local-only behavior preserved |
| `probe.status` | Admin fleet subscribers only; safe ProbeView subset |
| `monitor.probe.heartbeat` | Authorized monitor viewers; regional heartbeat DTO |
| `monitor.probe.status` | Authorized monitor viewers; monitor/probe identity, region freshness and status |
| `monitor.health` | Authorized monitor viewers; HealthView without unrelated fleet data |
| `probe.config.status` | Admin operation views; probe/revision/sync state and redacted validation errors |
| `probe.command.status` | Authorized operation requester/admin; command receipt |

Every payload containing a monitor ID is filtered through the existing AccessService monitor set. Administrative status streams require admin independently of monitor grants. Permission revocation takes effect on subsequent fan-out and refresh. No channel sends raw snapshot/provider configuration to browsers.

Use monotonically increasing `projection_version` to invalidate existing dashboard/Insights/navigation caches. The browser ignores older versions and resynchronizes after reconnect/gap. Batch regional updates per animation frame or bounded interval; never introduce a full-monitor-list query for every regional heartbeat. Tests must retain baseline query-count budgets.

## 9. Contract fixtures required before parallel implementation

M0 must check in valid and invalid JSON fixtures for every tabled message, enum, API shape, and configuration reference. Include maximum signed-64-bit strings and rejected overflow, null timestamps, empty authorized lists, unknown optional fields, invalid required capabilities, frame-size limits, and duplicate revisions with conflicting hashes.

Use those fixtures in Go transport DTO tests and frontend response validation/type tests. The current source verifies condition values in `internal/core/domain/monitor_condition.go`, maintenance minutes in `internal/core/domain/maintenance.go`, template fields in `internal/core/domain/notification_template.go`, and monitor-visibility acknowledgement authority in `internal/adapters/http/handlers/alert.go`. Preserve existing `alert.scope` (`monitor`/`group`) and add separate delivery-scope/probe variables as specified in the architecture. Any later correction updates this document in the same commit before independent agents consume it. No agent should infer field names from UI labels or domain struct names.


## 10. Implemented foundation limits and remaining freeze work

The initial implementation validates common envelope framing and observation-only telemetry batches, plus ACK/retry/gap payload structure. Recognizing a message name in an envelope does not validate that message's payload. Other durable event kinds are rejected explicitly by the observation decoder until their own schemas land. There is no listener, ingest transaction, authentication claim, or cursor advancement in these decoders.

Structural limits for this slice: JSON nesting 64; canonical lowercase UUIDs 36 characters; observation/condition messages 4096 UTF-8 bytes; condition metadata 256 bytes; at most two unique condition kinds per observation; at most 256 unique positive affected monitor IDs per gap; machine error codes at most 128 ASCII lowercase letters/digits/underscores. Existing 1 MiB frame, 512 KiB batch, 64 KiB event, and 256-event limits still apply. Transport DTO tests enforce required zero/false/null fields, duplicate object keys, canonical decimal values, status/count coherence, and positive entity IDs. Durable ordering against a stored cursor and assignment/config authorization belong to the later transactional ingest service.

M0 still needs full configuration and current-state DTOs/chunks, incident subject/lifecycle schemas, enrollment frames, command target fields, credential/reset HTTP requests, regional assignment read responses, and exact browser event views. It also needs the baseline HTTP/browser/template/maintenance/alert fixtures and all-message valid/invalid fixture matrix. Implement those before claiming complete protocol compatibility or enabling any remote capability.
