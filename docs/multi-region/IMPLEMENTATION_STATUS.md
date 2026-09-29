# Multi-region implementation status

Updated 2026-09-29 with the
[bounded populated partitioned migration rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md),
the [operator requirements](M6_OPERATOR_REQUIREMENTS.md) (egress, durability,
clock, disk, backup, provider reachability) and the
[fleet assignment-ownership gate](M6_FLEET_ACTIVATION_GATE.md) covering
verification matrix row T34, and with
[M5 completion](M5_COMPLETION.md) and
[executed evidence](M5_COMPLETION_EVIDENCE.json): fleet/enrollment UI, durable
revocation/delete, regional views and ACKs, authorized live updates and public
overall coverage. The next milestone is M6 validation and controlled activation.
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
| M6 release validation | In progress | [Fleet assignment-ownership gate (T34)](M6_FLEET_ACTIVATION_GATE.md), [operator requirements](M6_OPERATOR_REQUIREMENTS.md), and a [100k-row synthetic rehearsal of migration 037](M6_PARTITIONED_MIGRATION_REHEARSAL.md) recorded. Still open: the full section 13 matrix as one recorded run, production-sized partitioned migration and full-chain rehearsal with actual lock/disk impact, load envelopes and backlog drain rate, kill tests at every durable boundary, and the canary |
| M7–M9 | Future work | Optional aggregate paging, SSH provisioning and push gateway |

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
