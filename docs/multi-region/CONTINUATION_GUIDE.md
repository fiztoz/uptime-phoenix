# Multi-region continuation guide

**M5 continuation starts at [the foundation and frozen read contracts](M5_FOUNDATION.md).**
The first slice exposes scoped health and desired assignments. The second,
[fleet diagnostics](M5_FLEET_DIAGNOSTICS.md), adds the safe runtime diagnostic
read model and the admin `GET /api/probes` list/detail reads. The third,
[assignment writes](M5_ASSIGNMENT_WRITES.md), adds revisioned admin desired-set
replacement, atomic create-with-assignments and the clone rule. The fourth,
[administrative operations](M5_ADMIN_OPERATIONS.md), freezes the registration
network trust and lands registration writes plus durable enroll/rotate/reset
operations. Revocation, regional history, fleet UI and browser work remain; do
not infer completion from the full proposed route table.

M3 is complete at implementation `a12a3fa`, recorded in `8b455d4`; the first
M4 pull-checker slice is recorded in
[initial M4 acceptance](M4_PULL_CHECKER_ACCEPTANCE.md). The subsequent
[Docker binding slice](M4_DOCKER_BINDINGS.md) adds probe-local socket/API resources,
revisioned assignment references and runtime resolution. The
[TLS evidence slice](M4_TLS_EVIDENCE.md) retains remote certificate metadata in
source telemetry, hub history and current state. The latest
[certificate paging slice](M4_CERT_PAGING.md) lets a remote HTTPS monitor page its
certificate expiry from the source that owns the assignment. The latest
[capacity state slice](M4_CAPACITY_STATE.md) gives remote capacity conditions the
shared two-sample promotion contract with hub mirroring. The latest
[capacity paging slice](M4_CAPACITY_PAGING.md) pages confirmed capacity changes
and recovery as one source-owned incident per kind. The
[escalation slice](M4_ESCALATION.md) at `9046ee2` runs the accepted ladder on that source.
The [group and status-page recovery slice](M4_GROUP_STATUS_RECOVERY.md) makes
folder alerts and public status follow overall policy for remotely assigned
monitors. The [Insights slice](M4_INSIGHTS.md) invalidates Insights and
navigation caches with projection versions and ranks monitors that have
overall history from that history. The [backup/config slice](M4_BACKUP_CONFIG.md)
gives backup/restore and config-as-code stable probe keys and assignment
references, with restored identities disabled pending reenrollment. The
[lifecycle/recovery slice](M4_LIFECYCLE_RECOVERY.md) implements clear-history
watermarks with `history_cleared` receipts and defines the soft-delete,
tombstone, stream-retirement and restored-hub/edge recovery contracts. The
[deployment compatibility slice](M4_DEPLOYMENT_COMPAT.md) updates the operator
deployment docs and adds the Helm probes feature flag/config/secret references
without changing single-pod defaults or split-image behavior. The
[group-notification slice](M4_GROUP_NOTIFICATIONS.md) pages folder channels
from committed remote evidence and keeps those channels off regional
assignments. The [capability advertisement slice](M4_CAPABILITY_ADVERTISEMENT.md)
advertises compiled proxy protocols and rejects a proxy the checker would ignore.
The [maintenance and notification sync slice](M4_MAINTENANCE_NOTIFICATIONS.md)
keeps accepted schedules, direct links, templates, visibility, tags, owner and
escalation policy on the edge that executes them.
The [M0–M4 review fixes](M4_REVIEW_FIXES.md) preserve pending escalation delivery,
scope clear-history sequences to streams, defer restore activation and connect
regional ingestion to public incident recovery.
Read [current status](IMPLEMENTATION_STATUS.md) and
[M3 final acceptance](M3_COMPLETION_ACCEPTANCE.md)
before choosing work. Completed M2/M3 handoffs are not active assignments.

## Start here

1. Read [AGENTS.md](../../AGENTS.md), [testing](../TESTING.md), this status, and the
   relevant [architecture](ARCHITECTURE.md) and [protocol](PROTOCOL.md) sections.
2. Inspect HEAD, the working tree and current migrations. Preserve unrelated work;
   do not recreate completed types, enrollment, replay or recovery mechanisms.
3. Select a bounded requirement from [M4](IMPLEMENTATION_PLAN.md#7-m4--existing-feature-and-operational-compatibility)
   or [M5](IMPLEMENTATION_PLAN.md#8-m5--admin-api-and-regional-user-experience) under
   the user's requested scope. Those milestones are not authorized by an old M3 ledger.
4. Trace the production entry points and actual DTOs, freeze file ownership when
   delegating, and write effect-based acceptance before claiming completion.
5. Run the relevant gates from the testing guide, inspect actual MariaDB pass/skip
   events, and commit coherent source/tests/docs together. A local commit is not a
   push or deployment approval.

## Existing implementation map

| Area | Entry points |
|---|---|
| Composition and operator commands | `internal/bootstrap/`, `cmd/app/`, `cmd/probe/`, `cmd/phoenix-probe-admin/` |
| Runtime use cases | `internal/core/services/probe_*`, `internal/core/services/cert_alert_paging.go`, `internal/core/ports/` |
| Wire, TLS and sessions | `internal/adapters/probe/` |
| Hub authority and history | `internal/adapters/repository/probe_*`, `internal/adapters/repository/incident.go` |
| Edge persistence, cursor and retention | `internal/adapters/repository/edge/` |
| Production-process verification | `scripts/probe_runtime_smoke.py`, [testing guide](../TESTING.md) |

The [operator guide](M2_OPERATOR_GUIDE.md) covers manual enrollment, configuration,
watchdogs, ACK and rotation. [Reset acceptance](M3_HUB_RESET_ACCEPTANCE.md) records
the explicit recovery workflow; [key provisioning](KEY_PROVISIONING.md) covers
installation-key handling. Keep source identity and archived evidence intact.

## Constraints carried forward

Database authority, current-state transfer, historical ingest and provider sends
are separate effects. Configuration/application receipts follow commit. Queued
history must not create remote provider work on the hub. A delivered-threshold
cursor belongs to the source that sent the alert; the hub mirrors incidents and
outcomes instead of inferring one. Regional ACKs identify
their original incident. Cleanup preserves unresolved dependencies and tombstones.
No lease lock spans provider I/O. See [the retrospective](../postmortems/2026-09-21-m3-integration.md)
for the reproduced failures behind these rules.

M4 compatibility and M5 UI must preserve local-only defaults, current authorization,
JSON names and the locked monitor/provider inventory. Complete design tables may
include future behavior; the status and acceptance records define what runs today.
Historical checkpoint evidence remains committed. Superseded delegation and progress
notes are handled by [documentation maintenance](DOCUMENTATION.md).
