# Multi-region implementation status

Current results and accepted exceptions are consolidated in the
[October 4 validation summary](M6_VALIDATION_REPORT_2026-10-04.md).
Earlier checkpoints below retain their historical scope; M6 operational
acceptance remains incomplete. UAT/load campaign details stay local.

## M8 stage-admission increment — locally verified

Increment A's bounded local engineering verification is complete: one irreversible
DB admission of an exact root/systemd artifact attempt, followed by a constrained
injected Ed25519 signature. There is no production caller or persistent key
custody. This does not authorize the host helper, continuously fence an admitted
action, establish remote termination, or mark a resource applied.

The [verification workflow](../TESTING.md#m8-one-shot-stage-admission-increment-a-not-host-authorization)
passed on the final inputs with exact Go 1.26.9:

- **Real MariaDB + SQLite strict gate:** 306 named passes, four package lifecycles,
  zero failures/skips, all **63 required MariaDB leaves** and all **18 new SQLite
  leaves**, no lifecycle or missing-coverage errors. Darwin/arm64 race-enabled
  clients used a disposable Linux/arm64 MariaDB pinned to the approved public
  digest. Container identity/ownership-checked removal completed before gate success.
- **Full native race suite:** 6,834 named passes, zero failures, 565 named skips
  (560 missing-DSN, five audited optional/helper cases), 23 passed packages and
  14 no-test packages. This separate no-DSN run does not extend MariaDB coverage
  beyond the dedicated gate. Go/SQL/module/parser inputs remained unchanged.
- **Controls:** 44 offline Python tests; five SQLite semantic mutants caught by
  their intended assertions and ten unchanged-source controls passed; the new
  elapsed-budget/deadline tests passed ten race-enabled repetitions. Build/vet/fmt
  passed (vet/fmt fallback; golangci-lint unavailable).
- Two completed independent static reviews reported no blockers. The first's
  explicit budget-coverage gap and the second's fresh evidence-directory nit were
  addressed. Requested review model: `xiaomi-token-plan-sgp/mimo-v2.6-pro`;
  serving identity and runtime execution are not inferred from its reports.

An initial real-engine run had all required Go leaves pass but the gate failed:
its parser invented a parent event for the literal slash in a single `t.Run`
argument (`I/O`). The fixed parser uses the nearest emitted ancestor and still
requires the frozen repository root/engine lifecycles. The original failed result
is retained; a separately approved replacement canonical run passed on the fix.
The one pinned public image pull and both disposable runs were explicitly approved;
no host bind, existing DSN/credential use, VM management or pruning occurred.

Private evidence is retained under `tmp/m8-stage-admission-verification/acceptance-04/`.
This checkpoint records verification; see Git history for commit/publication
status. No deployment occurred. B/C host authorization/delivery, key/trust
installation, executor integration, Linux Go runtime acceptance and broad M6/M7/
release work remain open. The milestone below retains its broader incomplete scope.

## Earlier accepted checkpoints

Updated 2026-09-29. M5 is complete ([evidence](M5_COMPLETION_EVIDENCE.json)).
Bounded M6 evidence includes [T06 process kills](M6_VALIDATION_REPORT_2026-10-04.md),
[T20 edge ENOSPC](M6_VALIDATION_REPORT_2026-10-04.md), [T34 fleet awareness](M6_FLEET_ACTIVATION_GATE.md),
a [fresh dual-engine backend race run](M6_VALIDATION_REPORT_2026-10-04.md), a
[partitioned migration-037 rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md),
[Go-runner tail](M6_MIGRATION_RUNNER_REHEARSAL.md), and a
[published v0.4.5 → working-tree application upgrade](M6_VALIDATION_REPORT_2026-10-04.md).
The [operator requirements](M6_OPERATOR_REQUIREMENTS.md) record supported
bounds. M6 validation and controlled operator-owned activation remain open.
This follows the [M5 regional history routes](M5_REGIONAL_HISTORY.md),
following the [M5 durable administrative operations](M5_ADMIN_OPERATIONS.md),
the [M5 fleet diagnostics and admin read API](M5_FLEET_DIAGNOSTICS.md),
the [M5 read API foundation](M5_FOUNDATION.md) and the
[M0–M4 review fixes](M4_REVIEW_FIXES.md) for escalation
delivery, stream-scoped history clearing, restore activation and remote public
incident recovery. Earlier M4 maintenance and notification sync is recorded in
[maintenance and notification sync](M4_MAINTENANCE_NOTIFICATIONS.md). The previous
capability advertisement slice is recorded in
[capability advertisement](M4_CAPABILITY_ADVERTISEMENT.md). The previous
group-notification slice is recorded in
[group notifications](M4_GROUP_NOTIFICATIONS.md). The previous
deployment compatibility slice is recorded in
[deployment compatibility](M4_DEPLOYMENT_COMPAT.md). Accepted M3
implementation: `a12a3fa`; completion evidence: `8b455d4`.
This is the current status. Check HEAD and newer acceptance records before starting work.

| Milestone | Status | Evidence / next boundary |
|---|---|---|
| M0 contracts | Accepted engineering baseline | [M2 integrator acceptance](M2_ACCEPTANCE_REPORT.md) includes M0 regressions |
| M1 regional persistence and local parity | Accepted after integration corrections | [M1 retrospective](../postmortems/2026-09-20-m1-integration-followup.md), [M2 gate](M2_ACCEPTANCE_REPORT.md) |
| M2 autonomous runtime and enrollment | Accepted for HTTP/TCP/DNS | [M2 acceptance](M2_ACCEPTANCE_REPORT.md), [operator guide](M2_OPERATOR_GUIDE.md) |
| M3 synchronization and recovery | Complete for the supported runtime | [Final acceptance](M3_COMPLETION_ACCEPTANCE.md), [hashed evidence](M3_COMPLETION_EVIDENCE.json) |
| M4 compatibility | Pull-checker, TLS evidence, certificate paging, capacity state, capacity paging, escalation, group/status-page recovery, Insights, backup/config, lifecycle/recovery, deployment compatibility, group-notification, capability advertisement, and maintenance/notification sync slices accepted | [Initial coverage](M4_PULL_CHECKER_ACCEPTANCE.md), [Docker bindings](M4_DOCKER_BINDINGS.md), [TLS evidence](M4_TLS_EVIDENCE.md), [certificate paging](M4_CERT_PAGING.md), [capacity state](M4_CAPACITY_STATE.md), [capacity paging](M4_CAPACITY_PAGING.md), [escalation](M4_ESCALATION.md), [group and status-page recovery](M4_GROUP_STATUS_RECOVERY.md), [Insights](M4_INSIGHTS.md), [backup/config](M4_BACKUP_CONFIG.md), [lifecycle/recovery](M4_LIFECYCLE_RECOVERY.md), [deployment compatibility](M4_DEPLOYMENT_COMPAT.md), [group notifications](M4_GROUP_NOTIFICATIONS.md), [capability advertisement](M4_CAPABILITY_ADVERTISEMENT.md), [maintenance and notification sync](M4_MAINTENANCE_NOTIFICATIONS.md). M4 compatibility rows are accepted
| M5 fleet UI and API | Complete for V1 | [Completion](M5_COMPLETION.md), [hashed evidence](M5_COMPLETION_EVIDENCE.json), [wire contract](M5_COMPLETION_CONTRACT.md). Full dual-engine race suite, 18 browser cases and real pinned-TLS process partition/recovery acceptance passed |
| M6 release validation | In progress | See the [October 4 validation summary](M6_VALIDATION_REPORT_2026-10-04.md) for passed code checks, accepted exceptions and outstanding operational acceptance. |
| M7 consolidated paging | Operative backend merged; operational/release acceptance open | Foundations through `919ffd2` remain accepted at their historical scopes: [paging decisions](M7_PAGING_FOUNDATION.md), [desired mode](M7_DESIRED_DELIVERY.md), [wire compatibility](M7_PROTOCOL_DELIVERY.md), [protected receipts/readers](M7_RECEIPT_BACKED_CONFIG.md), [scoped diagnostics](M7_DELIVERY_CONFIG_EVIDENCE.md), [local diagnostics](M7_LOCAL_DELIVERY_CONFIG_EVIDENCE.md), [immutable assignment binding](M7_ASSIGNMENT_REVISION_BINDING.md), [pure cutover evidence](M7_CUTOVER_EVIDENCE.md), [real configuration sessions](M7_CONFIG_SESSION_BINDING.md), [edge](M7_EDGE_REGIONAL_APPLICATION.md)/[local](M7_LOCAL_REGIONAL_APPLICATION.md) regional facts, [regional reporting](M7_REGIONAL_APPLICATION_REPORTING.md), [remote](M7_DELIVERY_PREPARATION.md)/[local](M7_LOCAL_DELIVERY_PREPARATION.md) preparation, [shared rounds](M7_PREPARATION_ROUNDS.md), and [standby planning](M7_AGGREGATE_STANDBY.md). The new [operative cutover implementation](M7_OPERATIVE_CUTOVER.md) adds default-disabled all-source application, genuine aggregate delivery and provider journals; its strict full race gate passed 5,976 named tests / 570 MariaDB passes with all 181 M7 leaves and zero engine skips. The historical 5,742/525 standby checkpoint retains only its earlier scope. No deployment/live opt-in or release acceptance is claimed. Retention/accounting, operational gates, SSH installation and the push gateway remain follow-ons. |
| M8 SSH provisioning | Preflight, artifacts, journal, coordinator and one-shot SSH stage sender implemented locally; broad acceptance blocked; installation open | [Verified SSH preflight](M8_SSH_PREFLIGHT.md) remains available as a local, DB-independent CLI. [Asynchronous admin discovery](M8_ASYNC_PREFLIGHT.md) adds default-disabled API-local execution, secret-free dual-engine receipts, immutable retries, fenced expiry and joined cancellation. [Pinned binary preparation](M8_PINNED_BINARY_ARTIFACTS.md) adds offline explicit digest/size verification, Go/ELF consistency checks and private snapshots, with honest operator-supplied version provenance. [Digest-bound Docker metadata](M8_DIGEST_PINNED_IMAGES.md) additionally selects a unique platform manifest/config through a trusted index, emitting an immutable reference without registry/daemon contact or layer verification. Image `54fdc44`, journal `7b38761`, coordinator `0870c3a` and content `c983ec2` are published on `feat/m8-ssh-preflight`; publication is not deployment. The [internal install-intent/resource journal](M8_INSTALL_JOURNAL.md) records fenced one-shot reports and honest expiry/unknown history on both engines, while the [one-shot coordinator](M8_INSTALL_RUNNER.md) sequences fresh grants through an executor port only. No installed-success state or concrete installer endpoint exists. The [offline image-content verifier](M8_IMAGE_CONTENT.md) checks supplied layer hashes/sizes and bounded diff IDs, not rootfs/publisher/runtime identity. The new [local built-image gate](M8_IMAGE_CORRESPONDENCE.md) uses approved public pulls and two sequential actual platform exports, with inherited-config/layer checks and copied ELF identity. It found and fixes Dockerfile ARG defaults that put an amd64 executable under the arm64 label. The gate/fix are locally committed as `326ceda` (not pushed); final full-tree verification and cleanup are recorded there. The follow-on [private image snapshot](M8_IMAGE_SNAPSHOT.md) retains and re-verifies supplied metadata/layers, with one context-bound bundle and joined owned disposal; it is locally committed as `f1dd181` (not pushed), not a complete OCI export or actual delivery. The subsequent [closed single-platform archive](M8_IMAGE_ARCHIVE.md) borrows that snapshot, re-verifies the original graph and streams sealed deterministic tar bytes with a separately identified generated root. This archive checkpoint is locally committed as `b9cb6ed` (not pushed); importer compatibility and remote transport remain unverified. The follow-on [read-only binary staging inspector](M8_BINARY_STAGE_INSPECTION.md) observes a canonical operation/owner/attempt marker and requested binary size/hash through bounded SSH/SFTP, with no upload, exec, deletion, DB transition or startup wiring. Matching content does not prove ownership, fencing or cleanup authority. The next [fresh-only local binary writer](M8_LOCAL_BINARY_STAGE.md) creates the binary/marker beneath a borrowed trusted directory fd with exclusive no-follow writes, independent readback and conservative retained partial effects. This local seal is not a claim about /opt, SSH delivery, current DB authority or host fencing. The combined inspection/writer checkpoint is locally committed as `ce6e106` (not pushed); its shared-VM-held scratch remains retained. The next [exact-stream receiver](M8_BINARY_STAGE_RECEIVE.md) prepares raw bytes for a typed intent and confirms real EOF plus joined source closure before invoking the existing writer, without a new codec/port/process helper or remote transport. Receive is locally committed as `eb0dd00` (not pushed). The separate [one-shot local process helper](M8_BINARY_STAGE_HELPER.md) adds a strict bounded stdin frame and early config-free stage-binary dispatch under inherited directory fd3, with real EOF/joined owned input and parent-close-before-receipt. It accepts no parent pathname, resolves no identity-derived /opt target, opens no DB and acknowledges no applied resource; ordinary SSH invocation is not a trusted-fd launcher. That helper checkpoint is locally committed as `718e3b8` (not pushed). A separate [fixed Linux host-local launcher](M8_BINARY_STAGE_HOST_LAUNCHER.md) adds stage-binary-host, requiring effective uid0 and kernel-verified root-owned/non-writable literal root -> opt descriptors without fd3 or a destination argument. Its actual isolated Linux process/namespace effects remain distinct from physical/SSH host identity, current DB authority, a hostile-root/mount fence or an applied resource. The fixed host-local checkpoint is locally committed as `ac9c126` (not pushed). The subsequent [one-shot pinned SSH sender](M8_BINARY_STAGE_SSH_DELIVERY.md) delivers unchanged framing and exact prepared bytes to the trusted preinstalled absolute helper, requiring a strict matching receipt, normal exit0, joined local resources and live context. It and the subsequent restricted-key fixture are locally committed at `d159fd4`; there is no runner/HTTP/startup caller. Full native race 6,758 passes / 547 audited skips, focused 171, artifact 3,018, Python 19, 23 intended mutants and isolated Linux/arm64 actual OpenSSH 27 named passes / 24 required completions across three repetitions passed. All 1,535 prior source inputs remain unchanged. Host-key authentication is not helper/root-shell provenance, current DB authority or ownership fencing. Exec-attempt failures are Unknown; disconnect may complete stdin and stage bytes. No retry, remote termination, cleanup or applied-resource claim is made. A subsequent [restricted staging-key fixture](M8_BINARY_STAGE_SSH_AUTHORIZATION.md) tests native forced-command/restriction policy with generated unrestricted controls and actual Linux/arm64 effects: 36 required completions / 39 named passes over three repetitions. The unchanged production sender/helper gain no wrapper, caller or policy installer; full native race 6,758 passes / 547 audited skips, Python36 and 11 intended mutants passed with default/authorization post-mutation controls. All 1,535 committed source inputs remain unchanged; final audit/cleanup are separate, not new MariaDB or Linux-amd64 runtime acceptance. Client-visible handshake success is not privileged root authorization, and subsystem stderr is intentionally suppressed. Production delivery compatibility, trusted helper/key-policy installation, current-host fencing and installation remain open. No combined registry-index, publisher or runtime proof is inferred. None of these installs, enrolls or activates a probe. Actual image/layer delivery, host-side ownership/cleanup, bounded progress, complete Docker/systemd installation and full acceptance remain open. |
| M9 public push gateway | Not implemented | Remains a separate follow-on with no new public ingress. |

## Accepted M3 behavior

Complete protected configuration and durable application receipts; ordered replay,
explicit retention gaps and bounded storage; fresh current state ahead of history;
historical recomputation; independent connection watchdogs; durable incident ACKs;
credential/certificate rotation; explicit stream reset; and bounded shutdown.
Unsupported remote capabilities fail explicitly. Default local-only deployment
remains available without probes or new external services.

Final verification recorded 3,719 named race passes across 22 packages, zero
failures, two optional skips and 303 audited live MariaDB cases. CGO-free build
and lint passed. The actual 900.005-second outage passed 51 process stages and
replayed 1,440 retained events with fresh state first and one original ACK.
The final report distinguishes its combined partition run from the separate
credential/certificate/reset process acceptances. These are historical executed
results, not a claim that checks reran during documentation cleanup.

## Remaining scope

Every pull monitor type now has a remote runtime path. Docker requires an
explicit probe-local Unix socket or plain TCP API binding; Docker TLS/client-key
transport is not implemented. Remote TLS metadata now survives source restart,
ordered replay and current-state refresh with assignment-scoped storage. Remote
HTTPS monitors also page certificate expiry: the source evaluates its own
accepted assignment, holds the delivered-threshold cursor and open certificate
incident per assignment generation, sends through its own durable outbox, and the
hub mirrors the resulting incidents and delivery outcomes without inferring a
cursor or performing provider work. Certificate acknowledgement links remain
rejected by remote activation. Capacity state and paging now reach remote
execution: the source promotes ok/warning/error under the shared two-sample and
hysteresis contract, pages confirmed changes and recovery as one capacity
incident per kind through its own durable outbox, and the hub mirrors promoted
state, incidents and outcomes without recomputing promotion or touching
availability ([capacity state](M4_CAPACITY_STATE.md),
[capacity paging](M4_CAPACITY_PAGING.md)). Remote escalation now runs on the
source that owns the assignment: direct monitor policy, then nearest ancestor,
with a disabled policy stopping inheritance; acknowledgement and recovery cancel
a pending ladder ([escalation](M4_ESCALATION.md)). Public acknowledgement URLs
stay off. Folder alerts and public status pages now use overall policy for a
monitor assigned to a remote probe: a regional recovery does not close the
folder or a public incident unless that policy is a fresh up, and UNKNOWN stays
visible ([group and status-page recovery](M4_GROUP_STATUS_RECOVERY.md)).
Insights rankings and the folder/dashboard navigation caches invalidate on
projection version, and a monitor with overall history is ranked from that
history ([Insights](M4_INSIGHTS.md)). Backup/restore and config-as-code now
carry stable probe keys and complete assignment sets: restored identities are
created disabled pending reenrollment, an unrestorable set is never rerouted
to local execution, and prune never deletes probe registrations
([backup/config](M4_BACKUP_CONFIG.md)). Clear-history now removes the full
history scope behind a durable per-assignment watermark that fences replay,
and the soft-delete, tombstone, stream-retirement and restored-hub/edge
recovery contracts are defined ([lifecycle/recovery](M4_LIFECYCLE_RECOVERY.md)).
Operator deployment documentation and the Helm probes feature flag, config and
installation-key secret references are in place with single-pod defaults and
split-image behavior preserved
([deployment compatibility](M4_DEPLOYMENT_COMPAT.md)). A folder channel is not
copied onto a regional assignment. After a remote observation or current
snapshot commits, the worker that ingested it re-evaluates the folder from
overall policy and pages only that folder's channels
([group notifications](M4_GROUP_NOTIFICATIONS.md)). A proxy is advertised as
`proxy.<protocol>.v1` and is rejected before activation when the checker would
ignore it. Unsupported database engines fail the installed validator; they do
not get separate wire names ([capability advertisement](M4_CAPABILITY_ADVERTISEMENT.md)).
A published snapshot keeps the maintenance schedule, direct links, provider and
template settings, target visibility, tags, effective owner and escalation policy
the edge executes. Hub and probe binaries embed the IANA database
([maintenance and notification sync](M4_MAINTENANCE_NOTIFICATIONS.md)). Fleet and
regional UI are complete in [M5](M5_COMPLETION.md). Watchdog ACK is
not supported by the regional positive-assignment-generation command target.
Provider delivery retains its documented external-acceptance ambiguity. A WAL
admission threshold is not a hard filesystem quota. Local acceptance does not
authorize push, deployment or a production migration.

Follow [the continuation guide](CONTINUATION_GUIDE.md) and
[implementation plan](IMPLEMENTATION_PLAN.md) for future work. Codex completed
integration; no file ownership remains assigned to Antigravity. Read
[the consolidated retrospective](../postmortems/2026-09-21-m3-integration.md) for
verified mechanisms and [documentation maintenance](DOCUMENTATION.md) for the
local archive and retained checkpoint evidence.
