# Multi-region implementation status

Started: 2026-09-13. Source baseline: `b706fb09` on `codex/multi-region-probe-plan`, based on application `5183093c`. This branch belongs in the ordinary repository checkout so subsequent agents can continue the same work.

## Current delivery

M0 and M1 are **in progress**, not complete. The foundation implements executable contracts, shared health rules, additive registration/assignment storage, and current-state snapshot decoding/assembly. It does not supply a running remote probe. No probe listener, enrollment endpoint, remote scheduler, connector, provider outbox consumer, remote ingest endpoint, or regional user interface is enabled.

| Surface | Implemented behavior | Remaining integration |
|---|---|---|
| Domain | Reserved `local` identity, registration/assignment types, positive revision/generation contracts, `UNKNOWN=4`, ANY/ALL policy and count/coverage types, regional observation/state/stream/incident/delivery/command types, overall projection/history/dirty-bucket types, persisted assignment/policy effective-time intervals | Historical pause and timing configuration |
| Retry | Pure `EvaluateRetry`/`EvaluateObservation` reused by `HeartbeatService.Record`; maintenance then retry; independent state inputs; local Record atomically commits heartbeat+observation+state for the `local` assignment with a stream-wide sequence; `PromoteCondition` is the shared consecutive/hysteresis rule | Do not dispatch regional incidents from the existing dispatcher; capacity and certificate state now use assignment-specific repository views; durable regional delivery remains open |
| Health | Pure complete-assignment ANY/ALL truth table, deadline freshness, explicit missing/future/invalidated evidence, paused counts, duration-based uptime/coverage; `MonitorHealthService.Current`/`History`/`ProjectCurrent`/`ProcessDirty` | Incident recovery, browser/HTTP consumers of UNKNOWN, and historical pause/freshness configuration |
| Protocol | Bounded envelope validation, all five telemetry kinds, ACK/retry/gap, complete state/config DTOs and transfer frames, bounded hash-checked staging, config reference/target/capability checks, revision comparison, hello/welcome/health and trusted handshake comparison, command/enrollment/rotation-reset request and receipt DTOs, admin/browser ProbeView/HealthView/assignment/regional heartbeat events, 306 valid/invalid fixtures plus baseline HTTP/browser documents | Remote config building/validation and atomic activation; environment readiness; authenticated sessions/leases; durable application receipts |
| Database | Migrations `035`–`047` on MariaDB/SQLite; local backfill; credential-free registration stores; atomic revision-checked assignment replacement; tombstones prevent generation reuse; `RegionalCommit`/`Ingest` persist per-probe observations, cursors, optional incidents, and dirty buckets; delivery outcomes correlate to stored transitions; monitor Create writes a local assignment in the same transaction; hub ClaimBatch/schedulers skip remote-only sets; heartbeats carry `probe_id` (default `local`) and rollups unique `(monitor_id,probe_id,bucket)`; overall snapshots and history intervals; atomic membership/policy history on initialization and replacement; capacity and TLS state keyed by probe/generation; durable local sequence allocator with atomic heartbeat/regional recording; availability attempt throttles and availability incidents keyed by probe/generation; escalation inherits alert identity and only executes current-local work; both source recording ports accept atomic availability incidents/intents; probe-scoped queue leases and atomic outcome receipts; stable source UUIDs and lifecycle versions on legacy alerts; encrypted immutable prepared config snapshots; consistent local-source construction and deterministic dependency closure; exact local prepared-revision semantic validation | Live lifecycle/outbox integration, remote config building/validation, environment readiness, key startup wiring and activation, edge DB, historical-generation ingest authorization |

Explicit key provisioning and protected file loading are implemented by the standalone
`phoenix-probe-key` tool and auth adapter. See [key provisioning](KEY_PROVISIONING.md).
Hub/worker startup does not consume a key yet; activation remains gated.

Registration metadata still conveys no authentication authority. SQLite/MariaDB `MonitorRepo.Create` now inserts the reserved local assignment in the same transaction, so create/clone/import/restore through that path cannot leave an unassigned monitor. Hub `ClaimBatch` and both schedulers skip monitors whose assignment set has no active `local` member; monitors with no assignment set keep today's local execution. Remote assignment replacement is still not exposed on HTTP routes.

## Concrete contract decisions

1. Observation conditions are raw samples. Current-state conditions require separate observed/candidate/promoted state; a first warning can have null effective state. Replay never reruns promotion.
2. Remote V1 acknowledgement links are deferred. Keep existing local opaque-token links; remote incidents are acknowledged through authenticated hub commands after mirroring. Remote snapshots resolve `include_ack_url` to false and must reject true at activation.
3. Existing gRPC checks can return raw PENDING. Preserve PENDING with zero consecutive failures, including transport validation.
4. Known duration is UP + DOWN; unknown duration includes UNKNOWN, PENDING, and paused time. Maintenance is separate. Uptime uses known time; coverage uses known + unknown. A zero denominator yields null. These are durations from policy-derived intervals, never pooled samples.
5. A future-dated observation is not fresh current evidence. At the exact freshness deadline, observed evidence becomes UNKNOWN, including an old maintenance observation. Later configuration-derived maintenance must evaluate the current schedule explicitly.
6. New dependency versions use the complete snapshot's positive revision. Direct notification IDs and per-link IDs must agree; escalation preserves existing target-inclusion behavior.
7. Preserve distinct wire dialects: existing HTTP uses lowercase status and `message`; legacy browser heartbeat uses `msg` and `paused`; probe observations use uppercase status. Reserving UNKNOWN does not change legacy mappings or authorize emitting it there.
8. Assignment sets contain at least one enabled registration. No-op replacement retains revision/generation; membership or health-policy change increments the set revision; retained members keep generations; remove/re-add increments the tombstoned generation. Revision mismatch and exhaustion return conflict.
9. Current-state transfers bind snapshot/stream/config identity and connection generation, expire at a fixed 60-second deadline, and discard staging on any failure. Assembly verifies the original bytes, complete schema, and begin/content metadata; it returns evidence only. `state.applied` requires the later durable projection transaction. Snapshots retain raw/candidate/effective conditions, including first-ever OK and warning hysteresis, without applying promotion again.
10. Regional incident subjects, scope, monitor/generation, and start time are immutable. Deliveries bind an incident transition and channel version. The wire can describe canceled unsent intents with attempt zero, but it cannot request a provider send or claim hub-owned aggregate/group incidents. Pending availability escalation ends on acknowledgement/resolution; auxiliary/watchdog incidents do not gain escalation through these DTOs.
11. Hello/welcome comparison requires trusted installation/probe/stream/lease expectations and exact supported capability requirements. Empty retained history uses `first_retained_seq: "0"`, including at sequence exhaustion. Health preserves role-specific nulls and readiness prerequisites; its cursor is diagnostic and cannot authorize queue deletion. A cursor below retained history needs later declared-gap reconciliation; a cursor above source history or desired revision below active config fails the handshake.
12. Config reference graphs are complete, including disabled entries. Maintenance applicability is expanded/clipped to the snapshot and matches assignment links in both directions. Dependency versions equal the snapshot revision; capabilities are an exact union. Configuration assembly preserves original bytes for hashes and checks the trusted target/bindings, while activation still requires checker/provider/template/schedule validation and an atomic revision/generation recheck.

## API/browser contracts and regional commit — 2026-09-15

M0 transport/API shapes are now executable: `views.go` decodes ProbeView, assignment replacement, HealthView, regional heartbeats (`message`, lowercase status), and section-8 browser events. Baseline documents under `testdata/v1/baseline/` bind existing handler field names (`accepted_statuscodes`, HTTP `message`, browser `msg`, `monitor_ids`, `access_code`). Domain types and `RegionalCommit`/`ProbeIngest` ports live in `internal/core`. Migration `036_probe_regional` adds observation/state/stream/command/dirty-bucket tables without changing heartbeat partitions or rollup unique keys. SQLite (and MariaDB when `TEST_MARIADB_DSN` is set) tests prove a local UP retry counter stays zero while a remote assignment reaches confirmed DOWN on the same timestamp.

Monitor create and hub scheduling now honor local assignment ownership. Migration `037_probe_heartbeat` backfills heartbeat and rollup `probe_id` to `local`, replaces rollup uniqueness with `(monitor_id,probe_id,bucket)`, and keeps heartbeat `PRIMARY KEY (id, time)` plus `PARTITION BY RANGE (UNIX_TIMESTAMP(time))`. Local `HeartbeatService.Record` writes `probe_id=local`. Existing HTTP heartbeat JSON is unchanged (no `probe_id` on that view). `phoenix.probe.v1` is not advertised as complete. No probe HTTP mutation routes were added.

## Heartbeat probe identity and rollup uniqueness — 2026-09-15

This slice continues regional persistence. Heartbeats gain `probe_id` (default `local`), nullable `stream_id`/`source_seq`, `assignment_generation`, `received_at`, and `config_revision`. Rollup tables gain `probe_id` and `unknown_count`. The unique key is `(monitor_id,probe_id,bucket)`; auto-increment `id` values are preserved. SQLite rebuilds the rollup tables because it cannot drop a table UNIQUE constraint. Down migration refuses while any non-local heartbeat or rollup row exists.

GetLatest/ListByMonitor remain monitor-wide so existing local readers keep today's behavior. Overall policy readers and incident/delivery persistence remain open. Do not advertise `phoenix.probe.v1` complete.

## Overall health readers — 2026-09-15

`MonitorHealthService.Current` loads the assignment set, persisted regional state, and freshness window, then reuses `EvaluateMonitorHealth`. Local UP plus confirmed remote DOWN is DOWN under `any_down` and UP under `all_down`. Missing assignment evidence is UNKNOWN. No-grant callers receive `ErrNotFound` before any regional read. `ListStates` is scoped to one monitor; `ListObservations` forces UTC bounds and keeps `observed_at, id` order. Existing HTTP/browser status mappings are unchanged and still do not emit UNKNOWN.

## Incident and delivery persistence — 2026-09-15

Migration `038_probe_incidents` adds `probe_incidents` and `probe_delivery_events` without changing the existing `alerts` table or its one-open-alert-per-monitor uniqueness. Source `source_alert_id` is unique; `hub_incident_id` is the hub mirror. `RegionalCommit` persists an optional incident in the same transaction as observation and state, so an invalid incident rolls back the sample. Two probes can hold independent firing incidents for one monitor. Same-version puts are idempotent; lower versions and immutable identity changes conflict. Aggregate/group incidents are rejected. Delivery rows correlate to an already stored transition, may advance `attempt`, and never send a provider request. Down migration refuses while any incident or delivery row exists. The existing dispatcher is unchanged.

## Local regional recorder — 2026-09-15

`HeartbeatService.Record` commits local observation+state through `RegionalCommit` when a local assignment exists. Retry state prefers the local regional counter so a remote sample cannot recover a local DOWN. Sequence uses `domain.LocalStreamID` and increments from persisted state. Legacy monitors without an assignment set keep heartbeat-only writes. Existing HTTP/browser status mappings and the dispatcher are unchanged; Record does not persist regional incidents.

## Shared observation evaluation — 2026-09-16

`EvaluateObservation` applies accepted maintenance then `EvaluateRetry`. `HeartbeatService.Record` consults the maintenance schedule so push/API recording matches scheduler MAINTENANCE heartbeats; checker `raw_status` is preserved on the regional sample. `PromoteCondition` is the pure consecutive-sample and hysteresis rule used by `MonitorConditionService`. Capacity conditions remain monitor-scoped. The existing dispatcher still owns notification I/O.

## Overall projections and dirty-bucket history — 2026-09-16

Migration `039_probe_health_projection` adds `monitor_health_state` and `monitor_health_history` without changing heartbeat partitions or rollup keys. Regional commit/ingest mark 1m/1h/1d and overall dirty buckets in the same transaction as the sample. `ReconstructOverallHistory` builds duration intervals from regional transitions, assignment effective times, and freshness expirations; added probes do not participate before their assignment. `MonitorHealthService.History` is the overall historical read (access-checked, UTC-clipped). `ProjectCurrent` materializes the current snapshot and increments `projection_version` on change; `ProcessDirty` recomputes closed overall minutes. Local `Record` projects current overall health when the projector is wired. Down migration refuses while any snapshot or history row exists. Existing HTTP/browser mappings still do not emit UNKNOWN. Assignment effective-time persistence after remove/re-add is supplied by the next slice below.

## Assignment and policy effective-time history — 2026-09-17

Migration `040_probe_assignment_history` adds paired MariaDB/SQLite membership snapshots with generation, set revision, policy, and half-open UTC boundaries. Monitor creation/local initialization and assignment replacement persist the timeline in the same transaction as the desired set and mark the affected overall minute dirty. Retained members keep their generation; re-added members receive a new one. Policy-only changes open a new revision; no-op replacements leave the timeline untouched. Microsecond boundaries strictly increase across rapid edits and hub clock rollback. Reads order by `started_at, id`, normalize bounds to UTC, and preserve original interval boundaries.

`MonitorHealthService.History` and `ProcessDirty` use persisted membership instead of applying today's assignment set to all history. Removed generations remain in historical reconstruction; new generations cannot borrow their evidence. Missing assignment history now contributes UNKNOWN duration with `missing_assignment_history`, rather than disappearing from coverage. Neighboring intervals with different regional counts remain separate. Legacy-local fallback applies only when the monitor has no assignment set.

Upgrade with all application writers stopped. Backfill seeds only the current revision starting at the set's recorded last edit; older membership/policy cannot be recovered from tombstones. Migration clears the derived `monitor_health_history` cache, which may have been calculated under today's policy; direct historical reads reconstruct from retained source evidence. Existing raw observations, heartbeat IDs/partitions, rollups, and incident records remain intact. Down migration accepts untouched local revision-one membership only and refuses changed or remote history. Probe registration display/enabled edits, monitor pause/freshness configuration history, and historical-generation ingest authorization remain separate work.

Tests cover remove/re-add, policy-only transitions, generation recovery without changing prior durations, concurrent replacements, no-op/stale revisions, forced history-write failures (including monitor-create rollback), UTC and half-open bounds, missing-history coverage, schema constraints, safe local downgrade, guarded changed-history downgrade, and conservative/idempotent backfill. The same repository contract targets SQLite and MariaDB; live MariaDB requires `TEST_MARIADB_DSN`.

Small existing lint issues in the foundation were also cleaned up: the sequence exhaustion comparison is exact, the health evaluator no longer returns an unused monitor, and the secret scanner has no unused allow-list parameter.

Verification passed: `go build ./...`; the full `go test -race -count=1 ./...` suite and all 251 frontend unit tests through `make test`; full golangci-lint with zero issues; formatting, core dependency boundaries, whitespace, and documentation links/fences. `govulncheck ./...` found zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). SQLite ran the real persistence/migration cases. Live MariaDB was skipped because `TEST_MARIADB_DSN` was unset and no Docker daemon was available; its contract and populated migration rehearsal remain required before rollout. Frontend source and Helm were unchanged.

## Regional capacity and certificate state — 2026-09-17

Migration `041_probe_auxiliary_state` extends `monitor_conditions` with `(monitor_id, probe_id, assignment_generation, kind)` identity and TLS uniqueness with `(monitor_id, probe_id, assignment_generation)`. Existing observations, certificate IDs/metadata, promotion counters, and notification cursors backfill to `local` generation one. They are not relabeled as evidence from a later assignment. SQLite rebuilds both tables and preserves the TLS auto-increment high-water mark; MariaDB preserves TLS IDs and replaces the old monitor-only unique key after installing its replacement. Stop application writers for upgrade and downgrade. Down migration preserves local generation-one data and refuses while either table contains remote or later-generation state.

The two small regional repository factory ports return views bound to a probe and generation. Bound reads, writes, lists, and condition deletes cannot cross that identity. Unbound reads expose only the active local generation; absent assignment sets retain the legacy generation-one fallback. Removed local assignments return no evidence. Unbound writes reject remote-only monitor assignments. Unbound condition cleanup remains monitor-wide so disabling a condition removes all stored generations; monitor deletion cascades all regional auxiliary rows. Binding scopes storage only: it does not authenticate a caller or authorize execution.

`MonitorConditionService.OnAssignmentCheck` reuses the existing pure promotion/hysteresis rules with independent counters, last-success timestamps, notification cursors, and locks. `CertificateAlertService.OnAssignmentCheck` reuses the threshold/renewal rules against its bound view. Both are owning-worker operations, never hub replay operations. Alert context carries probe/generation and regional delivery scope separately from the existing monitor/group scope. Remote condition updates do not enter the legacy monitor-only browser event. Region names/template variables and regional browser delivery remain later integration.

`HeartbeatService.Record` passes the current local assignment to TLS metadata persistence and both evaluators. Re-adding local begins fresh capacity/certificate/retry state while retaining the observation sequence from its earlier persisted local state. Existing local thresholds, maintenance suppression, failed-send retries, recovery, and ordinary metadata refresh behavior remain covered. No new endpoint, remote scheduler, provider outbox, or incident-dispatch integration is enabled. These auxiliary writes and notification cursors still use the existing best-effort local flow; they are not yet atomic with the regional observation or durable delivery intent. External notification delivery remains at-least-once across a crash after sending.

Repository contracts run the same tests against SQLite and MariaDB: interleaved regional warning/error/OK samples; hysteresis and recovery; recreated services; independent certificate thresholds/renewal; failed and maintenance-suppressed delivery; current-local reads after remove/re-add; empty read scopes; scoped and administrative cleanup; UTC round-trips; local `Record` integration; populated up/down migration and downgrade guards. Verification passed: full `go test -race -count=1 ./...` plus 251 frontend unit tests through `make test`; final focused race tests after input-validation coverage; `go build ./...`; full golangci-lint with zero issues; formatting, core dependency boundaries, whitespace, and documentation link/fence checks. `govulncheck ./...` found zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). SQLite executed the persistence and migration cases. `TEST_MARIADB_DSN`, a local MariaDB binary, and the Docker socket were unavailable, so live MariaDB and populated rollout rehearsal remain required. Frontend source and Helm were unchanged.

## Durable availability attempt throttles — 2026-09-17

Migration `043_notification_throttles` adds `(monitor_id, probe_id, assignment_generation)` attempt cursors with UTC microsecond timestamps on MariaDB. The shared `NotificationThrottleStore` implements the small `Reserve`/`Clear` port. Reservation first writes/locks the key, then reads its current cursor and commits an eligible attempt in the same transaction; MariaDB uses a current locking read and SQLite takes its writer lock before reading. Concurrent resend callers on independent connections cannot both reserve one interval. Failed writes roll back the placeholder as well as the cursor. Forced transitions bypass resend delay but never lower the cursor when the wall clock moves backward.

Bootstrap wires durable storage into the existing dispatcher for both engines. Each local heartbeat supplies its assignment generation; legacy missing identity/generation maps to local generation one. Maintenance, acknowledged incidents, and disabled resends do not reserve attempts. Recovery clears only the matching assignment. Provider failures still consume the interval, preserving existing attempt-based backoff. The process-local fallback uses the same scoped key and an atomic check/update for isolated callers; production uses the database. Remote, mismatched-monitor, negative-generation, and nil heartbeats are rejected before group/lifecycle/provider work.

This slice does not scope legacy `alerts` or `alert_escalations`, change policy precedence/step-zero ownership, enable remote delivery, or integrate notification intents with heartbeat commits. A reservation is not a delivery receipt or an execution lease. A crash after reservation but before send can delay delivery until the next enabled resend; with resends disabled it can leave the initial alert unsent. A repeated transition is still owned by scheduler execution guarantees, not deduplicated by this throttle. Scoped lifecycle and transactional incident/outbox integration remain required. Storage keys alone do not authorize an inactive assignment. Existing ephemeral timestamps cannot be recovered during upgrade, so an initial immediate resend is possible. Downgrade refuses to discard any retained cursor; stop all writers before upgrade/downgrade.

Contracts cover restart through a second database connection, independent monitors/probes/generations, exact resend boundaries, clock rollback, UTC storage, twelve concurrent callers, provider-failure backoff, maintenance, acknowledgement, disabled resend, recovery isolation, invalid identity, failed-write rollback, monitor deletion, idempotent upgrade, and guarded downgrade. Verification passed on Go 1.26.6: focused race contracts; full `go test -race -count=1 ./...` and 251 frontend unit tests through `make test`; `go build ./...`; full golangci-lint with zero issues; formatting, core dependency boundaries, whitespace, and documentation link/fence checks. `govulncheck ./...` found zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). SQLite executed the persistence/migration contracts; live MariaDB cases were skipped because no test DSN was configured. Populated MariaDB upgrade/rollback rehearsal remains required before rollout. Frontend source, Helm, and dependencies were unchanged.

## Local stream allocation and atomic recording — 2026-09-17

The prior `HeartbeatService.Record` selected sequence one for each monitor's first check, but all local monitors share `domain.LocalStreamID` and observations enforce `UNIQUE(stream_id, seq)`. The new two-monitor regression reproduced a conflict on the second monitor. Its legacy heartbeat had already been saved, leaving a partial record. A control with two different sequences at the identical timestamp and stream passed, as did the existing one-monitor regression; timestamp precision and assignment creation were not the cause.

Migration `042_local_stream_sequence` adds the singleton `probe_local_sequence` high-water mark. Upgrade seeds it from the greatest retained sequence in local-stream observations, regional state, or legacy heartbeat rows. Reapplying the up migration never lowers it. Monitor deletion and history retention do not delete this counter. Upgrade/downgrade requires all writers stopped; down migration refuses any nonzero high-water mark even if source history was deleted. Historical rows, heartbeat IDs, source uniqueness, partition expressions, and rollup keys remain intact. The migration does not delete or relabel partial heartbeat records left by the old path.

The small `LocalHeartbeatRecorder` port is implemented by the existing shared MariaDB/SQLite regional store and wired in bootstrap. `CommitLocalHeartbeat` takes the allocator write lock before reading state, checks the active assignment and expected prior state sequence, then commits the allocator, heartbeat, regional observation, regional state, and all four dirty markers in one transaction. MariaDB takes current locking reads for assignment/state; SQLite acquires its writer lock before any read. It returns the stored heartbeat only after commit. Every rejected or failed transaction rolls back its sequence increment. No provider or EventBus I/O runs inside the transaction. Existing explicitly sequenced local-stream commits also lock and advance the allocator; remote ingest rejects reserved local probe/stream identities.

`HeartbeatService.Record` retries stale state evaluations up to 16 times using the original result/time and a fixed assignment generation. This preserves consecutive failures when two workers finish the same monitor concurrently. Assignment removal/replacement, allocation exhaustion, and storage failure return errors before any check hint or alert is emitted. A removed local assignment cannot fall through to legacy-local recording. Legacy monitors without an assignment set retain heartbeat-only recording; local legacy evidence can seed generation one's initial retry state, while remote heartbeat rows cannot. New generations still reset retry state and last-success history, and ordinary failures preserve last-success time within a generation.

Real-engine contracts cover the two-monitor reproduction, same-timestamp control, eight writers on two independent connections, concurrent same-monitor retries, forced failures at heartbeat/observation/state/dirty inserts, stale evaluation rejection, active-generation guards, UTC and source identity consistency, reopening a recorder after monitor deletion, the exact signed-64-bit maximum, exhausted/missing counters, conservative/idempotent migration seeding, and guarded downgrade. Unit tests prove a stale evaluation produces no intermediate heartbeat/event, cannot retarget a result to a new assignment generation, and a failed commit publishes nothing.

Verification passed on Go 1.26.6: `go build ./...`; full `go test -race -count=1 ./...` and 251 frontend unit tests through `make test`; full golangci-lint with zero issues; formatting, core dependency boundaries, whitespace, documentation links/fences, and paired migration checks. `govulncheck ./...` found zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). The first full run exposed an older ingest test using the reserved local identity as a remote fixture; it now creates and assigns a real remote probe, preserving its duplicate/gap/cursor assertions. The final full run passed. SQLite executed the persistence and migration contracts. Live MariaDB cases were skipped because no test DSN was configured; populated MariaDB upgrade/rollback rehearsal remains required before rollout. Frontend source and Helm were unchanged.

The atomic boundary now includes heartbeat and regional recording. Auxiliary certificate/capacity writes, overall projection, incident transitions, and notification intents remain separate follow-on work. Provider crash/retry semantics and regional dispatcher ownership are not completed by sequence allocation. The global counter serializes local recording transactions; production-scale throughput remains a rollout measurement, not a claim from these correctness tests.

## Command and enrollment contract continuation — 2026-09-15

This slice continues from the configuration contract. `commands.go` defines closed `command.request` / `command.result` DTOs for all seven V1 kinds, target/assignment/incident rules, and secret-free prepare details. `enrollment.go` defines generation-zero `enroll.request` / `enroll.result` frames and the write-only HTTP enrollment token body. `admin.go` defines rotate/revoke/reset-stream request bodies plus operation, command, and revoke receipts. Envelope recognition includes the enrollment message names; only `hello` and enroll frames may use connection generation zero.

Added 60 fixtures (285 total). Tests drive the shipped decoders from those original-byte files: every command kind has valid and invalid fixtures, signed-64-bit versions survive round trip, `local`/nil identities and enrollment-as-runtime tokens fail closed, and result/receipt JSON cannot carry `token`, `enrollment_token`, or `private_key`. No Echo route, probe listener, or scheduler was registered. A valid receipt shape is not a 2xx for unperformed work.

## Configuration contract continuation — 2026-09-14

This slice continues from `bd83bf7`. `config_snapshot.go` defines explicit configuration DTOs and bounded decoding; `config_dependencies.go` preserves existing timing, provider, maintenance, proxy, and escalation fields; `config_references.go` checks the complete dependency graph. `config_transfer.go` defines all five transfer/receipt/rejection frames and pure revision comparison. `config_assembler.go` stages original bytes, handles retries/cancellation/deadlines, and checks target identity, generation, capability union, and local Docker binding kind/key before returning a candidate snapshot.

Added 60 fixtures (225 total), including complete HTTP/Docker dependencies, SMTP/Discord/Webhook template settings, per-link target visibility, disabled-policy suppression, explicit maintenance applicability, maximum versions, dangling/conflicting references, and valid/invalid transfer frames. Tests cover every required nested schema member outside extension objects, typed round trips, the 10,000-assignment limit, collection/byte limits before nested DTO allocation, same-revision retry/conflict, out-of-order chunks, hash/schema/metadata failures, target/binding mismatches, fixed deadlines, cancellation, and no partial result on failure.

The checker/provider/template `config` fields are explicit bounded extension objects. Existing checker/provider validation, template placeholder/rendering rules, cron/timezone availability, proxy behavior, accepted configuration construction, and the atomic activation transaction remain required integration work. Shape recognition never claims that a monitor can execute. A structurally valid `config.applied` receipt does not prove persistence; the pure same-hash comparison cannot send one. No database schema, frontend, Helm, runtime route, scheduler, provider send, or activation path changed.

Verification passed on Go 1.26.6: `go build ./...`; full `go test -race -count=1 ./...` and 249 frontend unit tests through `make test`; full golangci-lint with zero issues; clean formatting, core dependency boundaries, and whitespace; all 225 JSON fixtures and documentation links/fences/examples. `govulncheck ./...` reported zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). The earlier registry foundation still requires live database rollout/migration rehearsal; this slice changes no persistence schema.

## Handshake and health continuation — 2026-09-14

This slice continues from `7730b30`. `handshake.go` defines bounded hello/welcome DTOs, explicit Docker binding inventory, and a pure transcript comparison against trusted caller expectations. `health.go` defines role-specific health evidence, explicit nullability, readiness coherence, and bounded redacted diagnostic codes. No capability registry or authenticated connection is enabled by these helpers.

Added 53 fixtures (165 total) and tests for installation/stream/generation mismatches, exact capability/version matching, required/nullable fields, inventory limits, immutable source counters at signed-64-bit maximum, empty queues, source clock rollback, readiness combinations, typed round trips, and common envelope guards. Successful transcript validation does not authenticate a peer, acquire/fence a lease, activate config, acknowledge/delete queued evidence, or establish monitor freshness. Those remain M2/M3 integration responsibilities.

Verification passed on Go 1.26.6: `go build ./...`; the full `go test -race -count=1 ./...` suite and all 249 frontend unit tests through `make test`; full golangci-lint with zero issues; clean formatting, core dependency boundaries, and whitespace; all 165 JSON fixtures and documentation links/fences/examples. `govulncheck ./...` found zero reachable vulnerabilities and zero advisories in imported packages (three module-level advisories outside imported/called code). Database schemas, production runtime paths, frontend source, and Helm are unchanged. Live database rollout/migration rehearsal remains an outstanding gate for the earlier registry foundation.

## Incident telemetry continuation — 2026-09-14

This slice continues from `ea7b444`. `incidents.go` defines availability/capacity/certificate/watchdog subjects, acknowledgement metadata, escalation progress, delivery outcomes, and promoted-condition transitions. `TelemetryBatch` now carries a closed set of typed payloads for every V1 telemetry kind; mixed batches preserve source ordering without repeating health evaluation or notification delivery.

Added 46 fixtures (112 total) and focused tests for mixed same-second evidence, lifecycle/version preservation, explicit nullable fields, certificate thresholds, scope rejection, delivery attempts, source clock rollback, exact signed-64-bit values, and common batch/event limits. A malformed suffix returns no usable partial batch. These are wire contracts only: immutable incident/delivery identity, prior authorized incident lookup, channel/assignment authorization, and atomic receipt/cursor/projection writes remain M1/M3 work. Database schemas and production runtime paths are unchanged.

Verification passed on Go 1.26.6: `go build ./...`; full `go test -race -count=1 ./...` plus the existing frontend unit suite through `make test`; full golangci-lint with zero issues; clean formatting, core dependency boundaries and whitespace; all 112 JSON fixtures and documentation links/fences. `govulncheck ./...` found zero reachable vulnerabilities (three module-level advisories outside imported packages/called code). Live database rollout/migration rehearsal remains a separate outstanding gate.

## Current-state contract continuation — 2026-09-14

This slice continues from `833dd6b` in the ordinary checkout. `state.go` implements the complete current-state schema, with explicit nullable promoted conditions and bounded identity/sequence/status checks. `state_transfer.go` implements the four typed frame decoders. `state_assembler.go` implements single-session staging, duplicate handling, byte/hash checks, metadata matching, timeout, and cancellation. Shared condition measurement validation preserves the existing observation wire names.

Added 41 golden fixtures (66 total), including first warning/error, confirmed warning, recovery, hysteresis, stale evidence, empty snapshots, and exact maximum sequences. Tests cover out-of-order delivery, identical/conflicting retries, fixed deadlines, cancellation, incomplete/corrupt transfers, metadata/generation conflicts, reconstruction-time duplicate keys, state/count/byte bounds, and required/null fields. No schema migration or production runtime path changed.

This completes the current-state wire/assembly subset of M0, not the full milestone or M3 reconciliation. Missing-assignment UNKNOWN handling, authorized stream/config/assignment validation, per-assignment sequence guards, durable application receipts, and projection writes still require the later service/repository implementation.

Continuation verification passed on Go 1.26.6:

- `go build ./...` and the complete `go test -race -count=1 ./...` suite.
- `make test` also ran the existing frontend unit suite: 249 passed, zero failed.
- Full golangci-lint: zero issues; `gofmt -l internal/` and whitespace checks clean.
- `govulncheck ./...`: zero reachable vulnerabilities; three module-level advisories did not affect imported packages or called code.
- All 66 protocol JSON fixtures, documentation links, and fenced examples validated.

Frontend source, Helm, and database schemas were unchanged. This verification does not replace the outstanding populated MariaDB migration/rollback rehearsal for the earlier registry foundation.

## Migration safety

Migration 035 creates `probes`, `monitor_probe_assignment_sets`, and `monitor_probe_assignments`. It seeds the reserved local row and revision/generation-one local assignments for existing monitors. It does not change heartbeat partitions, rollup keys, IDs, or existing scheduler queries.

Down migration is allowed only for untouched local-only foundation data. Remote registrations (even disabled), remote tombstones, or changed assignment revisions/policies prevent downgrade through a database constraint before the source tables are dropped. Stop all application writers for downgrade: MariaDB DDL auto-commits. A failed guard is a refusal, not permission to remove the check or discard records. Populated real-MariaDB migration/rollback rehearsal remains mandatory before deployment.

## Verification record — initial foundation

The unmodified baseline passed Go 1.26.6 build, the full race suite, and golangci-lint 2.12.2 with zero issues. That suite covers existing retry/recovery, condition promotion/hysteresis, certificate delivery cursors, maintenance suppression, acknowledgement/escalation, folder transitions, and WebSocket query budgets.

Integrator verification passed with Go 1.26.6:

- `go build ./...`.
- `go test -race -count=1 ./...` across the complete backend suite.
- `golangci-lint run --timeout 5m`: zero issues; formatting checks also clean.
- Six real SQLite registry/assignment contract cases, including two independent database handles for concurrent replacement, rollback after partial insertion, migration backfill, schema constraints and guarded downgrade.
- Protocol decoders: 25 golden fixtures plus required/null, size/depth, duplicate-key, sequence and status-coherence tests. Independent race review passed.
- Health truth table, freshness boundaries, paused counts, duration coverage and independent regional retry inputs; existing heartbeat/dispatcher regressions preserved.
- Documentation links, fenced JSON examples, whitespace and the exact changed-file manifest validated.

Lint identified one deprecated `bun.In` call; it was replaced with the repository's existing `bun.List` convention. Build/lint and the focused registry race suite passed again after that final adjustment.

Frontend/Helm changes are absent. Live MariaDB/MongoDB tests were skipped because the required environment variables/services were unavailable. SQLite success does not establish MariaDB execution coverage; a populated real-MariaDB migration/rollback rehearsal remains required before deployment.

## Next implementation steps

The Colima validation continuation (2026-09-17) closes the skipped real-MariaDB
repository gate and fixes two existing sharded-worker defects described below.
It added no migration after `043`. The scoped alert lifecycle/escalation continuation below adds `044`.

The local stream sequencing blocker is fixed by `042`; `043` adds durable assignment-scoped availability attempt throttles. Scoped alert lifecycle and local escalation ownership are implemented by `044`. Migration `045` adds atomic source incident/intent storage and fenced outcome completion; `046` adds stable source UUIDs and lifecycle versions to legacy alerts. Continue with complete accepted configuration snapshots (including channel versions) and transactional local lifecycle planning before activating an outbox consumer. The existing dispatcher now explicitly rejects remote heartbeats before any side effects.

1. M0 wire/API/browser fixtures, atomic commit/ingest ports, monitor-create local assignment, hub scheduler ownership, heartbeat `probe_id`/rollup unique `(monitor_id,probe_id,bucket)`, overall health readers, incident/delivery persistence, local `RegionalCommit`, shared retry/maintenance/condition evaluation, materialized overall projections/dirty-bucket history, and persisted assignment/policy effective-time history are in place. Do not advertise full `phoenix.probe.v1` capability. Capacity and certificate state are now scoped. Availability attempt throttles are now persisted and scoped. Alert lifecycle and escalation ownership are now scoped. Atomic incident/intent storage is available through both recording ports, and legacy alerts now retain stable source identities. Remaining M1 work is wiring local lifecycle planning and provider delivery to that storage, including versioned channels and obsolete-intent reconciliation; dedicated edge-schema migrations belong to M2.
2. Recheck HEAD before reserving numbers after `046_alert_source_identity`. Heartbeat partition expression and rollup auto-increment IDs are preserved; regional readers still mix monitor-wide heartbeat history until they are scoped.
3. Local monitor creation and scheduler ownership already enforce assignments. Preserve those atomic paths and gate remote assignment activation until every worker runs compatible code; never let old workers run remote-only monitors.
4. Prove T01/T02/T24/T31/T33 against real persistence. T03 is covered by `037_probe_heartbeat`. Current overall ANY/ALL readers and historical overall reconstruction exist; update all UNKNOWN consumers before emitting the new status on existing routes. Incident/delivery rows are stored but not wired into the existing dispatcher. Assignment effective-time history now supports remove/re-add reconstruction; historical ingest authorization and pause/freshness configuration history remain open.
5. Only then start the M2 edge runtime and authenticated transport. Leave SSH provisioning and public push gateway for their follow-on milestones.

Use disjoint file ownership, update shared contracts before delegation, and commit each tested slice. The initial helper/persistence work is not evidence that offline replay, notification ownership, or failure recovery already works.

## Colima MariaDB and running-app validation — 2026-09-17

Ran the repository contracts on disposable Colima MariaDB **11.8.9** (`mariadb:11`,
image digest `sha256:8b5f33ebd85d1775657e974ed10434128bb493c80e826ceaa54074fd1a92a112`).
The first run exposed failures that SQLite had hidden:

- `ClaimBatch` wrote fractional UTC time into second-precision `leased_at`, then
  selected its receipt by equality against the fractional input. One persisted
  lease produced zero returned monitors. MariaDB now locks candidate rows and
  updates/returns their exact IDs in one transaction. A timestamp precision
  control and bounded same-owner reclaim test cover the failure mechanism.
- The legacy matrix reset truncated the reserved local registration and sequence
  seed. Reset now restores migration-owned singleton data for every fixture.
  Other fixtures now accept MariaDB's case-normalized partition expression,
  provide the required observation message, and downgrade later dependencies
  before exercising isolated migration `035` rollback guards.
- `ShardedScheduler.tick` selected every active monitor despite claiming a lease
  batch. It now uses `WorkerMonitorReader` for the worker's active, unexpired
  leases, normalizes the cutoff to UTC, and keeps the local-assignment filter.
  Missing scope or failed reads cannot fall back to global execution. Both
  repository engines cover other owners, expired/missing leases, inactive
  monitors, the exact expiry boundary, and non-UTC callers.

The repeatable [real-app smoke script](../../scripts/multi_region_smoke.py) starts
two native app processes against Colima MariaDB with one monitor leased to each.
It uses actual HTTP checks and local webhook recipients. Verified effects:
independent scheduled checks; UP → PENDING → DOWN; one initial webhook per
monitor; a delivered escalation; acknowledgement canceling the remaining ladder;
new DOWN checks after restarting both processes without duplicate notifications;
then recovery webhooks and two resolved alerts. The unacknowledged second monitor
proves restart suppression comes from the durable throttle, not acknowledgement.

Readback found 23 heartbeats and 23 distinct contiguous local-stream sequences
(`1`–`23`) across both monitors and process restarts; the allocator was `23`.
Every heartbeat matched a persisted regional observation. Recovery cleared both
notification throttle rows. No Redis or external notification service was used.
Reproduction commands are in [TESTING.md](../TESTING.md#26-colima-multi-region-runtime-smoke).

Verification passed with Go 1.26.6: full `go build ./...`; complete
`go test -race -count=1 ./...` with `TEST_MARIADB_DSN` set (both repository engines
executed); a final concurrent-claim race test; golangci-lint with zero issues;
formatting, production core dependency boundaries, whitespace, script syntax,
and documentation links/fences. `govulncheck ./...` reported zero reachable
vulnerabilities and zero in imported packages (three module-level advisories
outside imported/called code). Frontend source, Helm, dependencies and schemas
were unchanged. This continuation did not run browser or frontend gates.

This validates current local execution and synthetic migration fixtures. It does
not establish remote probe operation, Redis/split-mode fan-out, or rollout safety
on an operator's populated installation. Worker selection at a tick does not fence
a check already in flight when ownership changes. Remote generation fencing,
scoped alert lifecycle/escalation, and atomic incident/delivery integration remain
open work; M0/M1 are still in progress.


## Assignment-scoped availability incidents and escalation — 2026-09-18

Migration 044 adds probe/generation identity to alerts and replaces monitor-only open-incident uniqueness with (open_monitor_id, probe_id, assignment_generation). Existing alerts remain local generation one, preserving incident IDs, acknowledgement tokens/times, resolution times and escalation IDs/progress/leases. SQLite rebuilds both the parent and its dependent escalation table with foreign keys enabled, preserving both AUTOINCREMENT high-water marks, including deleted rows. The migration runner now commits each SQLite migration and its tracking record in one transaction; a forced failure after the rebuild proves parent, child and schema rollback. MariaDB alters the parent in place. Stop all writers for upgrade/downgrade. Downgrade refuses any remote or later-generation incident, including resolved history.

Both SQL adapters use the shared AlertStore. Bound views confine create, read, update and list to an exact probe/generation. Unbound views keep legacy HTTP/API behavior: local history by ID/token/list, active-local generation for open lookup and creation. Remote token lookup is disabled. Binding scopes storage only; it does not grant execution or caller authorization. Alert identity, token and outage start are immutable through Update. Conditional lifecycle writes prevent delayed acknowledgement from reopening a resolved incident, preserve a concurrent acknowledgement on resolution, and retain the first acknowledgement metadata on repeated writes.

The dispatcher checks the current local generation before group/lifecycle/throttle/provider work and binds lifecycle operations to the recorded heartbeat generation. Re-added local assignments open a fresh incident without inheriting an old acknowledgement. Existing local tokens acknowledge only their original incident. Recovery closes only its bound generation and clears only its existing scoped throttle. Older incidents remain retained history; removal does not manufacture a target recovery.

Escalation already has one progress row per alert ID, so identity is inherited from the now-scoped parent rather than copied into independently mutable columns. State creation rejects a mismatched parent monitor. Hub claims exclude remote incidents. The runner rechecks local assignment generation after claiming and cancels obsolete local ladders; a transient assignment-read failure sends nothing and leaves the ladder retryable. Escalation notification context carries probe/generation and regional delivery scope. Precedence, disabled-policy behavior, step-zero ownership and local acknowledgement/recovery cancellation remain unchanged.

Contracts cover independent simultaneous incidents, scoped reads/writes and empty-grant lists, cross-probe acknowledgement/recovery, removal/re-addition, retained old-token isolation, restart suppression, local-only claims, obsolete-ladder cancellation, concurrent open attempts, stale lifecycle writes, UTC, populated upgrade/down cycles, deleted-ID high-water preservation, guarded downgrade, FK cascades and injected rebuild rollback. No remote listener, edge scheduler, remote provider dispatch, or new HTTP/browser response is enabled.

These checks do not fence provider I/O already in flight when assignment ownership changes. Incident transitions, heartbeat recording and delivery intents remain separate transactions; the durable outbox and edge runtime remain the next integration work. Existing folder/status-page recovery remains on the legacy local path until overall-health consumers are integrated. Do not activate remote assignments or advertise complete multi-region support yet.

Verification passed on Go 1.26.6: full build; complete backend race suite with TEST_MARIADB_DSN set (SQLite and MariaDB both executed); all 251 frontend unit tests; final focused database race contracts after strengthening migration metadata/lease assertions; full golangci-lint with zero issues; formatting, core dependency boundaries, whitespace, documentation links/fences and paired migrations. govulncheck found zero reachable vulnerabilities and none in imported packages (three module-level advisories outside imported/called code). The existing two-process real-app smoke passed against a fresh MariaDB database: separate leases, one initial webhook per monitor, escalation, acknowledgement cancellation, restart suppression, and recovery webhooks/resolved incidents. Frontend source, Helm and dependencies were unchanged; browser/type/build/Helm gates were not rerun for this backend slice. A populated operator-installation rollout rehearsal is still required before deployment.

## Atomic source availability delivery storage — 2026-09-18

Migration `045_probe_delivery_outbox` adds source-owned work independently from
mirrored `probe_delivery_events`. Both recording ports accept optional availability
incident transitions and bounded channel intents. `CommitLocalHeartbeat` commits
the incident and intents with its sequence, heartbeat, observation, state and
dirty work. `RegionalCommit` includes them with its explicitly sequenced record.
A failure anywhere rolls back the complete write set; a rolled-back incident ID
is not returned to the caller. Existing calls without intents keep their behavior.

Each intent preserves its source incident/transition, probe/generation, channel
and configuration version, stream/sequence, check output/time/status and outage
timing. The context survives later incident updates and observation retention.
Channel versions must match the configuration revision, following D15; this is
consistency validation, not proof that the configuration was authorized/applied.
No provider configuration is copied into this table. Legacy alert IDs, tokens,
heartbeat partitions and rollup IDs are unchanged.

`DeliveryOutboxRepository` provides probe-scoped reads, bounded claims and fenced
completion. Due work uses deterministic time/ID order; a current assignment
generation is required for claiming. A restart can reclaim an expired lease with
a new token and attempt. Completion atomically writes the queue result and its
delivery outcome, rejects stale/expired attempts, and permits an identical receipt
retry. Retry scheduling is explicit; diagnostic codes cannot contain raw provider
output. Mirrored outcomes cannot enqueue or bypass source completion leases.
Downgrade refuses every retained queue row, including completed receipts; monitor
deletion cascades its source work and outcomes. Stop writers for migration.

The first MariaDB concurrency test exposed a query-plan difference hidden by
SQLite: `EXISTS` became a semijoin, locking the common assignment and sorting the
candidate range before applying the batch limit. Changing only the index did not
fix it. A scalar generation lookup plus an index matching due/creation/ID order
passed repeated races with eight consumers and two database connections. Other
contracts cover failed-write rollback, restart, exact lease/retry boundaries,
UTC microseconds, stale completion, immutable context, replay isolation,
remove/re-add, invalid configuration identity, migration cycles and FK cascades.

This slice completes the atomic storage boundary, not live provider delivery.
The existing `HeartbeatService.Record` still supplies no incident/intents and the
dispatcher still opens legacy alerts and sends directly. Its crash gap therefore
remains. Next: versioned channel/config ownership and local alert-to-source ID
mapping, followed by transactional lifecycle/throttle/escalation planning and a
consumer with backoff, obsolete-DOWN supersession and delayed summaries. Claiming
does not decide whether an incident still warrants sending, grant execution
authority, or fence provider I/O already in flight. Auxiliary conditions, remote
runtime and overall-health recovery integration remain separate work.

Verification passed on Go 1.26.6: build; complete `make test` backend race suite
with `TEST_MARIADB_DSN` set (SQLite and disposable Colima MariaDB both executed),
plus 251 frontend unit tests; golangci-lint with zero issues; formatting, core
dependency boundaries, whitespace, documentation links/fences and paired
migrations. `govulncheck` found zero reachable vulnerabilities and none in
imported packages (three module advisories outside imported/called code).
Frontend source, dependencies, Helm, HTTP routes and bootstrap wiring did not
change; browser/type/build/Helm and real-app provider smoke were not rerun for
this storage slice. A populated operator-installation migration rehearsal remains
a deployment gate.


## Legacy alert source identity — 2026-09-18

Migration `046_alert_source_identity` gives every retained alert an immutable,
unique source UUID and establishes its current lifecycle as version one. Existing
alert IDs, acknowledgement tokens, status/times, probe/generation and escalation
progress/leases are preserved. SQLite rebuilds the parent and escalation child in
one transaction, retaining both AUTOINCREMENT high-water marks. MariaDB alters in
place. Stop all writers before either migration direction; MariaDB DDL is not
transactional.

`AlertStore.Create` persists the source UUID with the legacy alert. Failed inserts
return no newly allocated identity. Acknowledgement and resolution increment the
stored version in the same conditional update as the lifecycle change. Repeated
transitions do not increment it; delayed acknowledgements cannot reopen recovery;
version exhaustion fails without changing state. Caller-supplied identity/version
changes cannot overwrite persisted values. `AlertSourceRepository` resolves the
UUID within the existing local or bound probe/generation scope. It grants no
authorization, and the source UUID cannot substitute for the secret ack token.
Existing HTTP views remain unchanged.

The down migration refuses to discard any source UUID referenced by
`probe_incidents`. Unreferenced mappings may be removed for an explicit stopped
rollback: re-upgrade creates fresh UUIDs and resets their source versions to one,
while retaining all legacy alert and escalation data. Source identities must not
be published independently of durable incident records. Normal startup does not
regenerate identities. Existing regional incidents are not guessed or retroactively
matched to legacy alerts.

Tests cover independent outages/probes/generations, source/ID lookup equivalence,
restart readback, token compatibility, insert/update failure injection, concurrent
acknowledgement, idempotent and stale transitions, version exhaustion, populated
migration cycles, rollback after SQLite parent/child rebuild, downgrade refusal,
constraints, FK deletion and deleted-ID high-water preservation. One new MariaDB
test initially compared in-memory nanosecond timestamps with second-precision
stored values; repeated source-versus-ID read controls isolated the test baseline,
which now compares persisted rows without changing production timestamps. The full
suite also caught MariaDB's `ON UPDATE` behavior touching `updated_at` during UUID
backfill. A repeated fixed-historical-time control isolated that effect; the
migration now explicitly preserves `updated_at`, and the regression test keeps
the historical timestamp.

This continuation completes source identity mapping, a prerequisite alongside
versioned configuration. It does not turn an alert transition into a regional
incident or queue entry. Source lifecycle and future escalation publication must
share transaction/version ownership with recording. The live dispatcher still
sends directly; its crash gap remains. Next: build and persist the complete
accepted configuration (D15 channel versions equal snapshot revision), then wire
transactional lifecycle/throttle/escalation planning and the outbox consumer with
obsolete-intent reconciliation. `HeartbeatService.Record` currently assigns local
`ConfigRevision=1`; it must take the accepted complete snapshot revision before
queuing versioned channels. Remote execution remains gated; M0/M1 remain in
progress.

Verification passed on Go 1.26.6: final build; complete `make test` backend race
suite with `TEST_MARIADB_DSN` set (SQLite and disposable Colima MariaDB executed),
plus 251 frontend unit tests; golangci-lint with zero issues; formatting, core
import boundaries, whitespace, documentation links/fences and paired migrations.
`govulncheck` reported zero reachable vulnerabilities and none in imported
packages (three module advisories outside imported/called code). The two-worker
real-app smoke passed local ownership, initial delivery, escalation, acknowledgement
cancellation, restart suppression and recovery. SQL readback showed distinct
source UUIDs and resolved versions two (unacknowledged) and three (acknowledged).
Its runtime Go code matches the final source; the subsequent SQL-only backfill
fix passed repeated populated migration tests and the final full suite.
Frontend source, dependencies, Helm and HTTP/bootstrap behavior are unchanged;
browser/type/build/Helm gates were not rerun for this backend continuation.
A populated operator-installation rollout rehearsal remains a deployment gate.


## Protected prepared configuration — 2026-09-18

Migration `047_probe_config_snapshots` adds immutable per-probe revisions to both
hub databases. `ProbeConfigService.Prepare` inspects a bounded complete document,
checks its trusted hub/probe target and exact-byte SHA-256, then encrypts before
storage. The AES-256-GCM adapter requires an explicit 32-byte key and binds
identity, revision, schema, hash and UTC microsecond source/effective times as
associated data. Every seal uses a fresh nonce. SQL receives ciphertext and
nonsecret metadata only; no channel credential copy is materialized.

The registration row serializes preparation, including concurrent first writes.
A higher revision requires a matching expected latest revision. A same-latest
metadata/hash retry returns the original ciphertext and stored time; older
replays, changed content and hub authority changes conflict. Failed writes leave
no revision or receipt. SQLite and MariaDB tests use separate connections and
competing writers for both local and remote probes. Explicit historical reads
retain exact original bytes after reconnect; latest reads never fall back after
wrong-key, metadata or ciphertext authentication failure.

The inspector reuses the full snapshot graph/version rules. Its separate internal
local decoder permits push monitors, direct Docker configuration without remote
resource bindings, and existing acknowledgement-link preferences. A trusted local
target is required. The remote V1 decoder still requires UUID probe identities,
pull monitors, Docker bindings and no acknowledgement URLs. Dependency versions
continue to equal the complete revision (D15), including disabled dependencies.

Preparation does not authorize or activate anything: schema validation cannot
prove assignment ownership or checker/provider readiness. No active pointer,
`config.applied`, live provider consumer, credential pruning or key rotation is
implemented. The key is injected, with no automatic generation or persistence;
provisioning, permissions and backup remain runtime prerequisites. Stop writers
for migration; downgrade refuses every retained snapshot. Existing default boot,
HTTP views, heartbeat partitions and legacy direct dispatch remain unchanged.

Next: build consistent complete snapshots from authoritative hub configuration;
validate checker/notifier/template/schedule/proxy/binding semantics; provision a
durable protected key; atomically activate with registration/session/assignment
fences. Then replace local `ConfigRevision=1` with the applied revision and join
source lifecycle/throttle/escalation planning to recording. A delivery consumer
must reconcile current lifecycle, assignment and applied channel configuration
before I/O, including obsolete-DOWN supersession. Reading prepared or historical
credentials must never substitute for that check. Remote execution remains gated;
M0/M1 are still in progress.

Verification passed on Go 1.26.6: build; complete `make test` backend race suite
with `TEST_MARIADB_DSN` set (SQLite and disposable Colima MariaDB executed), plus
251 frontend unit tests; golangci-lint with zero issues; formatting, production
core import boundaries, whitespace, documentation links/fences and paired
migrations. `govulncheck` found zero reachable vulnerabilities and none in
imported packages (three module advisories outside imported/called code).
Frontend source, dependencies, Helm, HTTP routes and bootstrap wiring did not
change. Browser/type/build/Helm and real-app provider smoke were not rerun for
this storage slice. A populated operator-installation migration rehearsal remains
a deployment gate.


## Consistent local configuration construction — 2026-09-18

`LocalProbeConfigBuilder.Prepare` builds a complete local snapshot from saved hub
configuration and stores it through the existing encrypted preparation service.
The source reader uses one transaction: explicit read-only REPEATABLE READ on
MariaDB and one SQLite read snapshot. A two-connection contract changes a monitor
and its notification together between source queries; the first read retains both
old values and the next sees both new values, including when MariaDB's session
default is READ COMMITTED.

Current local assignments retain their persisted generations and paused state.
Remote-only assignments and tombstones are excluded. A legacy monitor without an
assignment set fails preparation until explicitly initialized. The service resolves
inherited contact and direct-monitor/nearest-ancestor escalation; disabled and empty
policies still stop inheritance. Direct notification links retain `include_target`,
channels retain local acknowledgement preferences, and only referenced channels,
templates and proxies enter the document. Group notification links do not inherit.
All dependency versions equal the new complete revision. Sorted collections and
UTC microsecond timestamps give exact retries stable bytes; channel rotation at the
same revision conflicts, while a higher revision preserves prior encrypted history.

Tracing the live maintenance path exposed an inaccurate design assumption:
`MaintenanceService.SetMonitors` and both repository adapters treat an empty link
set as affecting nothing, not all monitors. The builder preserves this behavior,
clips linked windows to local assignments, retains disabled windows, and resolves
an empty timezone to UTC. D28 and the protocol/architecture now record that rule.
No runtime behavior was changed to accommodate the old design prose.

The source graph and final document are bounded; errors never relay raw SQL or
extension-decoder text. Explicit transport mapping excludes user/admin fields and
push tokens. Local push and direct Docker configuration retain their existing
snapshot dialect. The local connection watchdog is disabled. Schema remains at
047; this slice adds no migration, dependency, HTTP route or bootstrap consumer.

Preparation still grants no execution authority. The saved snapshot is consistent
at its read point, but may become stale before preparation or activation; current
source/assignment checks remain mandatory. Next: runtime checker/notifier/template/
schedule/proxy validation, durable encryption-key provisioning and atomic local
activation. Then replace local revision one and join lifecycle/throttle/escalation
planning with recording, followed by the reconciled delivery consumer. Remote
resource/watchdog configuration, construction and runtime remain gated. M0/M1 are
still in progress.

Verification passed on Go 1.26.6: full build; complete `make test` backend race suite
with `TEST_MARIADB_DSN` set (SQLite and disposable Colima MariaDB both executed),
plus 251 frontend unit tests; golangci-lint with zero issues; formatting, production
core import boundaries and whitespace. Focused contracts cover graph closure,
policy precedence, maintenance exclusion, pause/generation preservation, exact
retry, conflicting source edits, credential rotation, encrypted restart readback,
concurrent consistency, empty/legacy/disabled sources, missing references, integer
and object bounds, secret-safe errors and cancellation. `govulncheck` found zero
reachable vulnerabilities and none in imported packages (three module advisories
outside imported/called code). Frontend/type/build/browser/Helm and real-app provider
smoke were not rerun for this internal builder; live startup/dispatch is unchanged.
A populated operator-installation migration rehearsal remains a rollout gate.


## Local prepared-configuration semantic validation — 2026-09-19

`LocalProbeConfigValidationService.ValidatePrepared` authenticates and validates an
exact positive local revision. Latest/zero and remote targets are rejected. The
adapter rechecks the complete graph, uses installed checker/provider validators,
reuses notification-template CRUD rules without mutating caller data, loads
maintenance timezones and parses cron schedules, and checks proxy host syntax.
Paused monitors and disabled dependencies are validated. Scheduler timeout, TLS
and accepted-status-code overrides are applied before checker validation. Unknown
capability names/versions and missing runtime extensions fail closed.

The local push exception is explicit: inbound tokens remain in hub-owned storage,
so validation verifies the registered push implementation/capability without calling
its token-requiring validator. A push token embedded in the snapshot is rejected.
The local watchdog remains disabled. Cron checks reject impossible dates and the
host-specific Local timezone; proxy syntax preserves IPv6 and all three protocols.
Errors expose only fixed categories/indexes or service sentinels, never extension
diagnostics or secret values.

This is a read-only semantic gate. Success returns metadata, writes no applied
receipt, executes no checks or sends, and proves no network/resource access. It
inherits the scope of the existing extension validators. Inbound push identity,
ICMP privileges, Docker access and current configuration/assignment authority still
need activation checks. Schema stays at 047; no dependency, route or bootstrap
consumer was added. Source construction and schema-only preparation remain separate,
so invalid desired content can be retained without being mistaken for active work.

Next: durable encryption-key provisioning and atomic local activation with current
source/registration/assignment fences bound to the exact validated revision/hash.
Then replace local revision one, join lifecycle/throttle/escalation planning to
recording, and implement the reconciled delivery consumer. Remote construction,
resource readiness and runtime remain gated; M0/M1 are still in progress.

Verification passed on Go 1.26.6: full build/vet/backend race suite with
`TEST_MARIADB_DSN` set (SQLite and disposable Colima MariaDB both executed),
golangci-lint with zero issues, frontend type check with zero errors/warnings,
unit tests, production build, Prettier/ESLint, all 12 Chromium E2E tests, and the
full Helm render/topology matrix. `govulncheck` found no reachable vulnerabilities
and none in imported packages (three advisories in unused module code). Formatting,
production core import boundaries and documentation links/fences passed.

The initial `make gate-full` stopped at a pre-existing `vitest` import in
`web/src/lib/probe-views.test.ts`; this repository uses `bun:test` and does not
depend on Vitest. An isolated TypeScript repro failed only this file while a
neighboring Bun test passed with the same compiler flags. Changing the import to
`bun:test` fixed the repro and the full frontend type gate, without a new dependency.
Earlier backend-focused gates did not run the frontend type check, leaving this
module-resolution failure uncovered. The remaining
gate-full steps were resumed and passed; the already-passing backend checks were
not repeated for this test-only import correction. No operator-installation migration
rehearsal or remote runtime claim is implied by these results.


## Durable snapshot key provisioning — 2026-09-19

`phoenix-probe-key init` explicitly creates a cryptographically random 32-byte
installation key in an existing private directory. A mode-0600 staging file is
written, synced and closed before atomic no-replace publication; the staging link
is removed and the parent directory is synced before reporting success. Existing
keys, malformed files and symlinks are never replaced. Concurrent creators cannot
clobber the winner. Loading never generates or repairs missing key material.

`NewProbeConfigProtectorFromFile` validates the opened file's regular type,
owner/root identity, exact mode 0400/0600 and exact raw length, with a bounded
read. The parent must have trusted ownership and no group/other write permission.
Confined relative symlinks support projected Secret layouts; escaped/absolute
links are rejected. Linux and macOS supply ownership checks; other platforms fail
closed. Diagnostics omit paths and key bytes. The temporary key buffer is cleared
after constructing the existing protector.

The standalone tool parses `PROBE_SECRET_KEY_FILE` through `caarlos0/env`, with
an explicit `--file` override. `check` constructs the protector but does not
connect to a database or claim that a key matches retained ciphertext. Build it
with `make build-probe-key`; release images/archives do not yet include it.
[Provisioning and recovery](KEY_PROVISIONING.md) covers private files, projected
mounts, backups, interrupted publication, and why replacing a key is not rotation.

This completes the file-provisioning prerequisite. Hub/worker bootstrap still does
not consume the setting. Schema remains 047; no dependency, HTTP route, default
startup requirement or dispatch behavior changed. Next: wire the provisioned key,
authenticate retained snapshots before writing new ones, and atomically activate
the exact validated revision/hash with current source, registration and assignment
fences. Then use the applied revision for recording, integrate transactional
lifecycle/throttle/escalation planning, and implement the reconciled delivery
consumer. Remote runtime remains disabled; M0/M1 are still in progress.

Focused race contracts passed with SQLite and a disposable MariaDB: database/key
reopen preserves exact prepared bytes, another valid key fails authentication,
and failed decryption leaves retained ciphertext unchanged. Filesystem tests
cover missing/short/oversized/insecure files, FIFO/device/directory rejection,
projected links, directory ownership/permissions, cancellation, backup reload,
and 20 concurrent creators. Separate compiled CLI processes proved one winner
among eight creators, restart checking and no-overwrite behavior; a CGO-free Linux
build passed create/check/no-replace in the disposable Linux container.

The initial special-mode test assumed this host's filesystem retained setuid.
An independent chmod/stat control showed mode 4600 is stored as 0600, while 0640
is retained. The test now verifies the fixture and skips only an unsupported
special-mode case; ordinary permission cases remain mandatory and production
mode checks were unchanged.

Full `make gate-full` passed on Go 1.26.6 with `TEST_MARIADB_DSN` set: build,
vet, complete backend race suite (SQLite and disposable MariaDB), golangci-lint
with zero issues, frontend check with zero errors/warnings, 251 frontend tests,
production build, Prettier/ESLint, all 12 Chromium E2E tests and the full Helm
render/topology matrix. `govulncheck` found no reachable vulnerabilities and none
in imported packages (three advisories in unused module code). Final auth/CLI
race tests and a fresh build also passed after threading context through the
private directory-opening helper. Formatting, production core import boundaries,
whitespace and documentation links/fences passed. No operator-installation rollout
or remote runtime is claimed by these checks.
