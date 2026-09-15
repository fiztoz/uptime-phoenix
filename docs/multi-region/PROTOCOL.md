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

The example version is illustrative, not a release-number reservation. The actual registry advertises only tested capabilities available in that build/runtime. All hello fields are required. Hub, remote probe, and stream identities are canonical non-nil lowercase UUIDs (`local` never connects through this protocol). `agent_version` is 1–128 printable ASCII bytes without whitespace; it is diagnostic text, not an ordering or compatibility authority. `protocol_min` and `protocol_max` are positive signed-32-bit integers with min <= max. V1 negotiation requires that range to include 1.

`config_revision` is zero before the first durable activation. `last_created_seq` is the durable high-water mark, including events no longer retained. `first_retained_seq` is zero exactly when no telemetry events remain in the queue; otherwise it is positive and at most `last_created_seq`. This zero sentinel also represents an empty queue at the maximum sequence without computing an overflowing last-plus-one. Neither retained bounds nor health messages authorize a cursor jump: missing history still requires explicit persisted gaps.

Capability arrays contain at most 128 unique names, each 1–128 ASCII bytes with nonempty dot-separated components of lowercase letters, digits, underscores, or hyphens. Unknown advertised names are compatible inventory only. Every capability required by the hub's accepted configuration must be explicitly advertised, without prefix or version substitution; a V1 session also requires `snapshot.v1`. The caller derives requirements from capabilities it implements, never from untrusted advertisements. Missing requirements block negotiation/config activation, and unsupported required names cannot be treated as optional. These names describe execution protocol support, not new user permission flags.

`hello.resource_bindings` advertises only `binding_key` and `kind` (`docker_socket` or `docker_api`) from the operator's local resource-binding file. It contains no host path, secret, or arbitrary execution instruction. An admin maps a monitor's Docker requirement onto one advertised key in its probe assignment. The probe resolves that key into its locally configured socket/API resource; mismatched kind or unknown key rejects config activation. A probe with no Docker binding cannot accept a Docker assignment even if the checker code is compiled in.

The binding list is required, may be empty, and contains at most 128 entries with unique keys across both kinds. Each key is 1–128 ASCII bytes, starts with a lowercase letter or digit, and contains only lowercase letters, digits, underscores, or hyphens. This excludes paths and URLs. Advertising a binding neither grants a monitor assignment nor proves Docker checker availability.

`welcome.payload` has: `hub_id`, `probe_id`, `stream_id`, `selected_protocol` (integer), `connection_generation`, `committed_seq`, `desired_config_revision`, `heartbeat_seconds` (15), `max_frame_bytes` (1048576), `max_batch_events` (256), `max_batch_bytes` (524288), and `hub_time`. A stream unknown to the hub is rejected with `stream_reset_required`; a reset is an authenticated administrative action, not a client request to discard history.

Every welcome field is required. V1 selects protocol 1 and the exact limits above; its positive generation equals the envelope generation. `committed_seq` and `desired_config_revision` may be zero. Before accepting the pair, compare both frames with the authenticated installation/probe, durably registered stream, and the current DB lease generation. A copied/mismatched identity, unknown stream, or mismatched generation fails closed. The cursor cannot exceed the hello high-water mark, and the hub's desired config revision cannot be below the probe's active revision. A cursor below retained history is allowed so declared-gap reconciliation can proceed. Do not infer reset, rollback, config activation, queue deletion, or fresh monitor evidence from a successful pair validation. TLS/credential checks, lease acquisition/fencing, handshake deadlines, and durable reconciliation remain session/service responsibilities.

### 2.2 Application health

`health.payload` has `role` (`hub` or `probe`), `ready` (boolean), `db_writable` (boolean), `scheduler_healthy` (boolean or null for a hub API role), `config_revision`, `committed_seq`, `queue_bytes`, `oldest_queued_at` (timestamp or null), `clock_time`, and `errors` (bounded array of redacted error codes). Hub health describes ingest readiness; probe health describes local execution readiness. Values irrelevant to the sender are null, never fabricated zeros.

All fields are required, including nulls. Both revisions/cursors are nonnegative decimal strings: `config_revision` is the probe's durable active revision (or the hub's last durably acknowledged active revision); `committed_seq` is the hub's durable ingest cursor (or the probe's last durably processed telemetry ACK cursor). Zero means no such activation/progress yet. These are diagnostics only; receiving health never acknowledges events or activates config.

For `probe`, `scheduler_healthy` is a boolean and `queue_bytes` is a nonnegative signed-64-bit JSON integer for its durable telemetry queue. Zero bytes requires null `oldest_queued_at`; positive bytes requires a timestamp. For `hub`, queue bytes and oldest time are both null; scheduler health may be a boolean when reporting a scheduler or null when inapplicable. `ready: true` requires `db_writable: true`; a ready probe additionally requires healthy scheduling and a positive active config revision, even when that config assigns zero monitors. False readiness need not invent an error. The `errors` array contains at most 32 unique machine codes, each 1–128 lowercase ASCII letters/digits/underscores; it never carries raw exception text or credentials. Nonfatal diagnostics may coexist with readiness. Timestamps may move backward; no cross-field wall-clock ordering implies progress or freshness. The session must check sender role and connection generation on every health frame.

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

### 3.3 Bounded decoding and reference rules

Every member listed in section 3.2 is required, including empty arrays and explicit nullable references. Snapshot revision and assignment generations are positive. All dependency versions equal the enclosing revision. Bound the snapshot to 16 MiB, each collection entry to 256 KiB, assignments to 10,000, and each dependency collection to 1,000. IDs are unique and positive within their collection; binding keys follow the hello key grammar. Per-assignment link/maintenance lists and per-step/watchdog notification lists are unique positive IDs bounded to 1,000. Assignment notification IDs exactly match its link IDs. Each channel/template, assignment/proxy/policy, and policy/watchdog/channel reference resolves in this snapshot, including disabled entries. Template and channel providers must match.

Maintenance `monitor_ids` is the explicit set of assignments in this snapshot to which that window applies, including expansion of a global window and clipping to this probe. Its edges must exactly match assignment `maintenance_ids` in both directions. Single windows require non-null ordered start/end instants; cron windows require null dates, a nonempty cron expression, and positive duration. The builder resolves an empty legacy timezone to `UTC`; cron parsing, timezone availability, and actual schedule evaluation remain activation/runtime validation.

Monitor interval and timeout are positive, retry interval and retry count are nonnegative, and resend interval is nonnegative. Integer seconds/counts and fractional timeout are bounded to signed-32-bit maximum; minute durations/resends additionally fit `time.Duration`. Names are nonblank and at most 255 UTF-8 bytes; description/owner metadata is at most 16 KiB per field. Tags are at most 256 entries with unique nonblank names (255 bytes) and values of at most 4096 bytes. Accepted status-code entries retain the existing spelling, with at most 64 nonempty strings of at most 64 bytes; checker validation still defines their meaning. No defaults are invented by the decoder.

Monitor and channel `config` are required JSON objects of at most 64 KiB; template `config` is an object of at most 192 KiB, allowing the existing HTML/body layouts. These explicit extension fields retain provider/checker keys; arbitrary top-level fields do not enter typed DTOs. Templates retain the existing four supported providers (`discord`, `smtp`, `webhook`, `line`), 1000-byte title and 64-KiB body bounds. Their provider-specific settings and placeholder/rendering rules must pass the existing template validator before activation. Structural decoding is not proof that any checker, sender, template, cron, timezone, or proxy can run.

Escalation steps are a dense ordered 1..N list of at most 20 entries, each with at least one channel and `delay_seconds` in 0..604800 divisible by 60, preserving the existing 0..7-day minute delays. Empty/disabled policies remain valid and suppress inheritance. Watchdog loss/recovery durations are positive signed-32-bit seconds; resend retains minutes. All 12 existing pull monitor types and 11 existing providers are recognized; push remains invalid for remote assignments. Each assignment explicitly requires `checker.<type>.v1`. A Docker assignment has exactly one local resource binding; other monitor types have none. Both the key and kind must match the target runtime's inventory before a transfer can return a usable snapshot.

`config.begin.required_capabilities` is the exact unique union of `snapshot.v1`, every assignment's requirements, and `notifier.<type>.v1` for every included channel. The receiver compares these with its trusted runtime inventory; unavailable names fail closed. The reconstructed body must yield the same set, and its revision and effective time must equal the begin metadata. Its hub/probe IDs and envelope generation must match trusted target/session expectations. All five transfer frames require their listed fields. Chunks use the state-transfer canonical base64 and 1–1024/nonempty count bounds. Rejections contain 1–64 objects with a nonempty JSON-pointer-style path (at most 256 ASCII bytes), a machine code (128 bytes), and a redacted message (4096 UTF-8 bytes); never forward raw secret-bearing validator errors.

Pure revision comparison distinguishes a new higher revision from a same-revision/same-hash retry and rejects older revisions or same-revision/different-hash conflicts. It is not an activation receipt. Transfer assembly checks exact original bytes, bounds, capabilities, references, target, and metadata, then discards staging on success or any error. Fixed 60-second deadlines are not extended by duplicate chunks. The owner discards staging on cancellation/idle expiry. Persistence must recheck revision/generation under its activation transaction before it can send `config.applied` or skip an already durably applied snapshot.

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

`alert.transition` and `watchdog.transition` data contain `source_alert_id`, `scope`, `monitor_id`, `assignment_generation`, `status` (`firing`, `acked`, `resolved`), `transition_version`, `started_at`, `resolved_at`, `reason`, `config_revision`, `subject`, `acked_at`, `acknowledgement`, and `escalation`. All fields are required, including explicit nulls. Incident IDs are canonical non-nil lowercase UUIDs; transition/config versions are positive decimal strings. `reason` is redacted text bounded to 4096 UTF-8 bytes. A monotonic incident transition version orders mirrored updates; timestamps retain the source wall clock even when it moves backward.

`alert.transition` has scope `regional`, a positive monitor ID and assignment generation, and one of the monitor subjects below. `watchdog.transition` has scope `probe_connection`, null monitor/generation, and subject `watchdog`. The authenticated stream supplies the probe identity. Remote telemetry cannot claim hub-owned `aggregate` or group incidents. This delivery scope is separate from the existing template `alert.scope` (`monitor`/`group`).

Every `subject` object has required `kind`, `condition_kind`, `certificate_threshold`, and `certificate_not_after`:

| Subject kind | `condition_kind` | `certificate_threshold` | `certificate_not_after` |
|---|---|---|---|
| `availability` | null | null | null |
| `capacity` | `session_pool` or `storage` | null | null |
| `certificate` | null | integer 30, 14, or 7 | exact UTC certificate expiry |
| `watchdog` | null | null | null |

Subject, scope, monitor, generation, and start time are immutable for an incident ID. Each certificate threshold/expiry pair has its own incident identity; crossing a more urgent threshold is a new incident, with the old one resolved administratively. This keeps the existing 30/14/7-day delivery suppression rule expressible without changing an incident's subject. Renewal never reuses an earlier certificate incident ID. A capacity incident keeps its condition kind through warning/error changes and recovery. The later ingest transaction must enforce immutable identity and increasing transition versions against stored records.

For `firing`, `resolved_at`, `acked_at`, and `acknowledgement` are null. For `acked`, `resolved_at` is null and both acknowledgement fields are non-null. For `resolved`, `resolved_at` is non-null and the acknowledgement pair is either both null or both retained from the earlier acknowledgement. Acknowledgement metadata contains `command_id` (canonical UUID), `actor_display_name` (nonblank, at most 256 UTF-8 bytes), and `note` (required nullable string, at most 4096 bytes). No token, API key, or acknowledgement URL travels with an incident. Authenticating the acknowledgement command and actor belongs to the source and later hub correlation, not the DTO decoder.

`escalation` is null unless this is an availability incident with an effective escalation policy. A non-null object contains positive `policy_id`, positive decimal `policy_version` no greater than `config_revision`, `status` (`pending`, `done`, `canceled`), `next_step`, and `next_run_at`. Pending progress requires a positive integer next step and UTC next-run timestamp; completed/canceled progress has both null. Only a firing incident may carry pending progress. Step zero remains owned by the initial notification dispatcher. Capacity, certificate, and watchdog incidents do not gain availability escalation through this contract.

`delivery.result` data contain `delivery_id`, `source_alert_id`, `source_transition_version`, `notification_id`, `notification_version`, `event_kind`, `attempt`, `status` (`sent`, `retrying`, `failed`, `superseded`), and `error_code`. All are required; IDs are canonical UUIDs, entity IDs are positive integers, and versions are positive decimal strings. `event_kind` is `status_change`, `certificate_expiry`, `capacity_condition`, `probe_connection`, or `incident_summary`. The first three preserve existing provider event names; the last two describe watchdog and delayed-summary intent. Retries/resends do not invent a new availability state.

`sent`, `retrying`, and `failed` require a positive signed-64-bit integer attempt. `superseded` permits zero because an obsolete intent may be canceled before any provider attempt. `sent` and `superseded` require null `error_code`; `retrying` and `failed` require a bounded redacted machine code. A delivery ID belongs permanently to one source incident transition, channel/version, and event kind; attempts may advance without changing that identity. The ingest service must correlate the result with an already authorized incident transition (earlier in the same batch or committed previously) and the accepted channel configuration. Neither a DTO nor a retention gap authorizes a previously unknown delivery subject. It describes the source provider outcome; the hub never treats it as a send request.

`condition.transition` data contain `monitor_id`, `assignment_generation`, `config_revision`, `kind`, `previous_state`, `state`, `message`, and `source_alert_id`. All are required. Monitor/generation/config identities are positive. `kind` is `session_pool` or `storage`; `state` is the promoted `ok`, `warning`, or `error`, never derived stale. `previous_state` is a different confirmed state or null for the first promotion. The source sequence orders these transitions. `message` is redacted text bounded to 4096 bytes, and `source_alert_id` is a canonical UUID or null. A supplied incident reference must correlate with the same monitor/generation/condition at ingest. These transitions never change primary availability, create incident recoveries by inference, or rerun promotion.

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

Unlike raw observation conditions, each current-state `conditions` entry carries `kind`, `observed_state`, nullable `effective_state` (null means not yet confirmed), `consecutive_state`, `consecutive_count`, `last_success_at` (nullable), and `message`, `used`, `limit`, `percent`, `threshold`, `unit`, `resource`, `scope`, `source`, `observed_at`, `stale_after` with the same types and limits as raw conditions. There is no ambiguous `state` field in this DTO. Valid observed/effective/candidate states are `ok`, `warning`, and `error`; stale is derived. Hub ingest mirrors the effective state without rerunning promotion. Notification cursors remain source-owned and are not inferred from raw evidence.

The candidate is the source's result after hysteresis. It normally equals `observed_state`; raw OK may remain a warning candidate while an effective warning is latched. Counts are positive signed-64-bit JSON integers. At two or more samples, the effective state must equal the candidate. An unconfirmed first warning/error has count one and null effective state. First-ever OK is immediately stable with count two, preserving the existing evaluator. A measurement success (`observed_state` OK or warning) has non-null `last_success_at`; error may retain a prior success or have null. Do not compare success/observation/snapshot wall clocks to infer ordering: clocks can move backward. Freshness and clock-skew eligibility are later projection checks.

Each state has `monitor_id`, `assignment_generation`, `last_observation_seq`, `observed_at`, `status`, `down_count`, `ping`, `message`, `conditions`, `tls`, and `active_source_alert_id` (nullable canonical UUID for the availability incident; capacity/certificate incidents travel separately). All fields are required, including explicit empty arrays and nulls. Status is source evidence: `UP`, `DOWN`, `PENDING`, or `MAINTENANCE`, never fabricated UNKNOWN. UP/MAINTENANCE require zero `down_count`, DOWN requires a positive count, and PENDING permits zero (raw checker PENDING) or a positive retry count. A still-open incident may survive PENDING/maintenance; the decoder does not infer a recovery from its absence or presence.

`config_revision` is positive; `last_created_seq` may be zero for a stream without events. Each positive `last_observation_seq` is at most `last_created_seq`. A snapshot contains at most 10,000 unique positive monitor IDs, with one positive assignment generation each and no reused observation sequence across states. Each serialized state is bounded to 64 KiB. Omitted active assignments become UNKNOWN with `missing_snapshot_state`; they never retain fresh evidence indefinitely. Assignments without an observation are omitted. An empty snapshot is valid; assignment completeness and missing-state handling require the authorized assignment set at application time.

The hub validates assignment and config generations and atomically applies entries newer than their current per-assignment sequence. A state snapshot does not advance the historical telemetry cursor, create a raw heartbeat, infer missing transitions, or trigger regional provider delivery. Fresh snapshots can drive overall displayed health. Optional aggregate paging must use its own current-evidence reconciliation rules.

### 5.1 Exact state transfer frames

Every payload below includes `snapshot_id` (canonical non-nil UUID), `stream_id` (canonical non-nil UUID), and positive `config_revision` (decimal string). They also use the ordinary envelope and its positive `connection_generation`.

| Type | Additional required payload fields |
|---|---|
| `state.begin` | `state_schema_version` (integer 1), `created_at`, `last_created_seq`, `total_bytes`, `chunk_count`, `sha256` |
| `state.chunk` | `index` (zero-based integer), `data_base64` |
| `state.commit` | `sha256` |
| `state.applied` | `sha256`, `applied_at`, `state_count` (number of entries in the committed snapshot, including entries already newer at the hub) |

`sha256` is exactly 64 lowercase hexadecimal characters over the complete snapshot's original UTF-8 bytes. The snapshot's `stream_id`, `config_revision`, `created_at`, and `last_created_seq` must equal the begin metadata. Chunking never changes the JSON bytes used for hashing. `data_base64` uses canonical padded RFC4648 standard base64 without whitespace; decoded chunks contain 1–262144 bytes. Total snapshot bytes are 1–16777216. There are 1–1024 chunks; the declared count must be capable of covering the declared total with nonempty chunks of at most 262144 bytes. The schema decoder independently enforces UTF-8, duplicate-key, depth, per-state, count, and field bounds on the reconstructed JSON.

One state transfer may be staged per authenticated session. Chunks may arrive out of index order. Identical duplicate chunks are idempotent and do not extend the deadline; conflicting duplicates, a mismatched transfer identity/generation, malformed frames, excess bytes, incomplete commit, or a hash/metadata mismatch discard the staging transfer. It expires 60 seconds after begin, including at the exact deadline. Session cancellation discards it. Receivers use a local monotonic clock for this timeout. Reconnection starts a new transfer.

Assembly and typed decoding establish only structural validity. They neither apply a projection nor send `state.applied`. The later ingest service must authorize the authenticated probe/stream and assignments, apply the complete snapshot and missing-state effects transactionally with generation/sequence guards, and only then send `state.applied`. Repeated committed snapshots require a durable application receipt; an in-memory assembler is not that receipt. A failed or interrupted transfer changes no existing state.

## 6. Commands, enrollment, and rotation

### 6.1 Command contract

`command.request.payload` has `command_id`, `kind`, `created_at`, `expires_at`, `target`, and `data`. Allowed V1 kinds are `alert.ack`, `probe.stop`, `history.clear`, `credential.prepare`, `credential.activate`, `certificate.prepare`, and `certificate.activate`. Configuration changes use snapshots, not an unbounded generic command API. Command `data` is a closed object: only the members listed for that kind are accepted.

`command_id` is a canonical non-nil lowercase UUID. `created_at` and `expires_at` are UTC timestamps. `target` is required and has exactly these members:

| Target field | Rule |
|---|---|
| `probe_id` | Canonical non-nil lowercase UUID. `local` is invalid on the probe protocol. |
| `assignment_generation` | Positive decimal string for `alert.ack` and `history.clear`; must equal `data.assignment_generation` for `history.clear`. Null for `probe.stop` and credential/certificate commands. |
| `source_alert_id` | Canonical non-nil lowercase UUID for `alert.ack`; must equal `data.source_alert_id`. Null for every other kind. |

`command.result.payload` has `command_id`, `status` (`applied`, `already_applied`, `already_resolved`, `rejected`, `expired`), `applied_at` (nullable), `code` (nullable), redacted `message`, and `details` (nullable object). `applied` / `already_applied` / `already_resolved` require `applied_at` and null `code`. `rejected` requires a machine `code` and null `applied_at`. `expired` has null `applied_at` and a nullable `code`. `details` is non-null only for `applied` / `already_applied` credential or certificate preparation: credential preparation contains only `credential_version`; certificate preparation contains exactly `certificate_version`, `tls_fingerprint`, and `not_after`. The probe persists result identity before acknowledging. Retain command IDs through at least the command expiry plus the maximum supported reconnect/retention window. A repeated command returns its recorded result.

`alert.ack.data` is `source_alert_id`, `actor_display_name`, and `note` (`note` is required; use null when unused). It affects only that incident. `probe.stop` durably disables scheduling after accepted shutdown instructions. `history.clear` contains an explicit observation-time/sequence watermark and scope. Only defined fields are accepted; there is no shell execution command.

| Command kind | Exact data members |
|---|---|
| `alert.ack` | `source_alert_id`, `actor_display_name`, `note` (string or null) |
| `probe.stop` | `reason`, `effective_at`; only the bound hub/admin authority may issue it |
| `history.clear` | `monitor_id`, `assignment_generation`, `through_observed_at`, `through_seq`, `clear_id`; both bounds constrain discarded evidence |
| `credential.prepare` | `rotation_id`, `credential_version`, `token`, `overlap_expires_at`; token is a write-only `phx_probe_` runtime credential |
| `credential.activate` | `rotation_id`, `credential_version` |
| `certificate.prepare` | `rotation_id`, `certificate_version`, `valid_for_days` (positive JSON integer 1–3650); probe generates/persists the pending key/certificate locally |
| `certificate.activate` | `rotation_id`, `certificate_version`, `expected_fingerprint` (64 lowercase hex SHA-256) |

`rotation_id` and `clear_id` are canonical non-nil lowercase UUIDs. `credential_version` / `certificate_version` / `through_seq` / `assignment_generation` are positive decimal strings. No `command.result` or other read body includes `token`, `enrollment_token`, or a private key. A config snapshot with active assignments is the explicit way to resume scheduling after an administrative stop; a lost connection cannot undo a persisted pause.

### 6.2 Manual enrollment

1. Probe initialization writes stable identity and a self-signed TLS certificate to its protected data directory, then outputs the endpoint suggestion, certificate SHA-256 pin, and a 10-minute single-use enrollment token to the local operator. Output is shown once; server logs omit it.
2. An admin obtains those values through the VM console or verified SSH, creates a hub probe registration, and submits the token through the write-only enrollment endpoint.
3. The hub connects to `/ws/probe/enroll/v1` using the pin and enrollment token, presents its `hub_id` and assigned `probe_id`, and negotiates protocol/capabilities. The probe refuses binding to a different existing hub without explicit local reset.
4. Before sending a new runtime credential, the hub durably stores it encrypted with a stable `enrollment_id`. Probe durably records the runtime token hash and binding before consuming the enrollment token and responding.
5. Hub reconnects to the runtime endpoint with the persisted runtime token. If the enrollment response was lost, this reconnect recovers the completed exchange. If preparation failed before probe commit, the same unexpired enrollment ID/token may be retried. Expired uncommitted enrollment requires a fresh local token.
6. Hub marks enrollment active only after a runtime handshake succeeds. Monitor execution begins after the first valid config activation. No success is inferred from an SSH process exit code alone.

Enrollment application frames use the common envelope on `/ws/probe/enroll/v1` with `connection_generation: "0"`. The enrollment token authenticates that WebSocket (`Authorization: Bearer <enrollment_token>`) and is never a JSON member of these frames. Hub and probe identities are canonical non-nil lowercase UUIDs; `local` is invalid. Decoding these frames does not open a runtime session or mark enrollment active.

`enroll.request` is sent only after the hub has durably stored the new runtime credential under `enrollment_id`. `enroll.result` is sent only after the probe has durably recorded the runtime token hash and hub/probe binding. A lost result is recovered by reconnecting to `/ws/probe/v1` with that credential, not by treating the enrollment socket close as success.

| Type | Required payload fields |
|---|---|
| `enroll.request` | `hub_id`, `probe_id`, `enrollment_id`, `protocol_min`, `protocol_max`, `capabilities`, `credential_version`, `token` |
| `enroll.result` | `hub_id`, `probe_id`, `enrollment_id`, `status`, `credential_version`, `applied_at`, `tls_fingerprint`, `certificate_not_after`, `code`, `message` |

`enroll.request` members:

| Field | Type and meaning |
|---|---|
| `hub_id`, `probe_id`, `enrollment_id` | Canonical non-nil lowercase UUIDs. `enrollment_id` is stable across retries of the same unexpired token. |
| `protocol_min`, `protocol_max` | Positive signed-32-bit integers with min <= max. V1 requires the range to include 1. |
| `capabilities` | Same inventory rules as `hello`; V1 enrollment requires `snapshot.v1` exactly. |
| `credential_version` | Positive decimal string. |
| `token` | Write-only runtime credential. Prefix `phx_probe_` with at least 32 unpadded-base64url bytes; `phx_probe_enroll_` is invalid here. |

`enroll.result` members:

| Field | Type and meaning |
|---|---|
| `hub_id`, `probe_id`, `enrollment_id` | Same identities as the request. |
| `status` | `applied`, `already_applied`, `rejected`, or `expired`. |
| `credential_version` | Positive decimal on `applied` / `already_applied`; `"0"` when rejected or expired before a version was bound. |
| `applied_at` | Timestamp on `applied` / `already_applied`; null otherwise. |
| `tls_fingerprint` | 64 lowercase hex SHA-256 on `applied` / `already_applied`; null otherwise. |
| `certificate_not_after` | Timestamp on `applied` / `already_applied`; null otherwise. Refuse expired certificates at the later session; decoding only checks shape. |
| `code` | Machine error code on `rejected`; nullable on `expired`; null on success statuses. |
| `message` | Redacted string, never a token, PEM, or private key. |

No enrollment result includes `token`, `enrollment_token`, or a private key.

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
| `POST /api/probes/:probe_id/enroll` | Write-only body `{ "enrollment_token": "<phx_probe_enroll_…>" }`; returns 202 operation receipt |
| `POST /api/probes/:probe_id/rotate-credential` | Body `{ "credential_version": "<positive decimal>" }`; returns 202 operation receipt; no plaintext token in the request or response |
| `POST /api/probes/:probe_id/revoke` | Body `{ "reason": "<redacted string>" }`; returns 202 revoke receipt |
| `POST /api/probes/:probe_id/reset-stream` | Body `{ "stream_id": "<uuid>", "enrollment_operation_id": "<uuid>" }`; returns 202 operation receipt |
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

Enrollment tokens use prefix `phx_probe_enroll_`; runtime credentials use `phx_probe_` and must not use the enrollment prefix. Both suffixes are unpadded base64url of at least 32 cryptographically random bytes.

An operation receipt has exactly:

| Field | Type and meaning |
|---|---|
| `operation_id` | Canonical non-nil lowercase UUID |
| `probe_id` | Canonical non-nil lowercase UUID; `local` is invalid |
| `status` | `pending`, `running`, `succeeded`, or `failed` |
| `phase` | 1–128 lowercase ASCII letters, digits, or underscores |
| `created_at`, `updated_at` | UTC timestamps |
| `error` | Null unless `status` is `failed`; otherwise `{ "code": "<machine code>", "message": "<redacted>" }` |

A command receipt has exactly `command_id` (canonical UUID), `status` (`pending`, `applied`, `failed`, `expired`), and `remote_confirmed` (boolean). A revoke receipt is an operation receipt plus `remote_confirmed` (boolean): `true` only after the probe confirmed revocation; `false` means the hub recorded it and the remote side is unconfirmed. None of these receipts includes a token or private key. Feature-disabled handlers for these paths must not return 2xx for unperformed work.

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

The implementation validates common envelope framing and mixed telemetry batches containing all five V1 kinds: `observation`, `alert.transition`, `watchdog.transition`, `delivery.result`, and `condition.transition`, plus ACK/retry/gap payload structure. It also decodes complete current-state snapshots and all four state transfer frames, with bounded in-memory assembly as specified in section 5.1. Recognizing another message name in an envelope does not validate that message's payload. Unknown event kinds return an explicit unsupported error. There is no listener, ingest transaction, authentication claim, provider send, or cursor advancement in these decoders.

Structural limits for this slice: JSON nesting 64; canonical lowercase UUIDs 36 characters; observation/condition messages 4096 UTF-8 bytes; condition metadata 256 bytes; at most two unique condition kinds per observation; at most 256 unique positive affected monitor IDs per gap; machine error codes at most 128 ASCII lowercase letters/digits/underscores. Existing 1 MiB frame, 512 KiB batch, 64 KiB event, and 256-event limits still apply. Transport DTO tests enforce required zero/false/null fields, duplicate object keys, canonical decimal values, status/count coherence, and positive entity IDs. Durable ordering against a stored cursor and assignment/config authorization belong to the later transactional ingest service.

Hello/welcome/health DTOs and trusted transcript comparison are also implemented, with 53 additional fixtures (165 total). The comparison checks installation/probe/stream identity, the caller's lease generation, compatible V1 limits, exact capability requirements, source high-water bounds, and config revision direction. Health decoding checks role-specific nullability/readiness and bounded codes. These helpers perform no authentication, lease operation, durable mutation, watchdog scheduling, or queue acknowledgement.

Configuration DTOs, dependency/reference validation, all five transfer frames, pure revision comparison, and bounded assembly now add 60 fixtures (225 total). Decoding rejects unresolved dependencies, asymmetric maintenance applicability, missing target-visibility links, wrong dependency versions, unsupported remote types, and remote acknowledgement URLs. Assembly verifies the exact bytes, capability union, target/session identity, and local resource bindings. It returns a candidate only; it does not build desired config, invoke extension validators, schedule work, persist an activation, or issue an application receipt.

Command, enrollment, and rotation/reset HTTP request/receipt DTOs add 60 fixtures (285 total). Typed decoders cover all seven `command.request` kinds, `command.result` statuses and secret-free prepare details, `enroll.request`/`enroll.result` with generation zero, write-only `phx_probe_enroll_` / `phx_probe_` tokens, and operation/command/revoke receipts. Decoding these shapes does not apply a command, bind a hub, rotate a credential, or return HTTP 2xx for unperformed work.

Admin/browser views add 21 fixtures (306 total) plus baseline compatibility documents under `testdata/v1/baseline/`. Typed decoders cover ProbeView (including reserved `local`), fleet lists, create/patch, assignment replacement, HealthView, regional heartbeats that keep `message`, and the section-8 browser events. Existing HTTP heartbeat/monitor/alert/maintenance/template/`access_code` names and browser `msg` are captured separately so they cannot be renamed by accident. The config snapshot inventory test is the runtime-extension matrix for all pull checkers and notification providers.

Pure regional observation/state/stream/incident/command types and atomic `RegionalCommit` / `ProbeIngest` ports are defined. Local overall current/history projection writes exist. Authenticated session/deadline/lease integration remains M2 work. Config construction, checker/provider/template/cron/timezone validation, atomic activation, incident/delivery identity correlation and lifecycle monotonicity, state snapshot authorization, missing-assignment reconciliation, durable application receipts, and remote snapshot projection transactions remain M1/M3 integration work. Implement those before claiming complete protocol compatibility or enabling any remote capability.
