# Multi-region continuation guide

Current results and accepted exceptions are consolidated in the
[October 4 validation summary](M6_VALIDATION_REPORT_2026-10-04.md).
Earlier checkpoints below retain their historical scope; M6 operational
acceptance remains incomplete. UAT/load campaign details stay local.

For authorized M7 continuation, the [decision foundation](M7_PAGING_FOUNDATION.md),
[desired delivery persistence](M7_DESIRED_DELIVERY.md), and
[wire compatibility](M7_PROTOCOL_DELIVERY.md) are merged. The
[receipt-backed configuration read slice](M7_RECEIPT_BACKED_CONFIG.md), committed
as `db89b6c`, preserves desired mode through cold loads and reads exact acknowledged
documents. The [delivery configuration diagnostic](M7_DELIVERY_CONFIG_EVIDENCE.md),
committed as `284bc35`, adds an opt-in, scoped, nonsecret view for current remote
assignment generations. The follow-on [local document evidence](M7_LOCAL_DELIVERY_CONFIG_EVIDENCE.md)
uses the same stored receipt protocol for local assignments without rebuilding
mutable execution sources. The [pure cutover-evidence contract](M7_CUTOVER_EVIDENCE.md)
requires separate assignment/document/stream-bound mode-application evidence;
it has no aggregate application producer or production caller. The [immutable assignment revision binding](M7_ASSIGNMENT_REVISION_BINDING.md)
captures set identity in newly built documents and preserves unbound legacy reads;
it still grants no runtime-mode authority. The [Colima real-engine acceptance](M7_COLIMA_ACCEPTANCE.md)
now verifies all required retained-read/API/binding MariaDB markers without skips.
The local/cutover/binding foundation was committed as `d86e6f8`. The subsequent
[live configuration-session verification](M7_CONFIG_SESSION_BINDING.md) passes
all three desired-mode session leaves on real MariaDB, including refused receipt,
cold reopen, ABA, reconnect and repeated ACKs. Check the remaining release and
runtime-lifecycle limits before proceeding.
The [edge regional application seam](M7_EDGE_REGIONAL_APPLICATION.md) now persists
honest source-local effective regional facts and validates present facts during
recording, escalation enqueue and final authorization. It adds no reporting
protocol/capability, aggregate application or suppression. The corresponding
[protected local-hub seam](M7_LOCAL_REGIONAL_APPLICATION.md) adds atomic effective
regional state on fresh activation, present-state execution/recording checks and
normal new-revision upgrade for missing facts. Local receipt retries do not
backfill; ordinary legacy dispatch remains unchanged/unreported. Local pre-send
reads are observational, not a new final transactional lease/send fence.
The [authenticated regional reporting slice](M7_REGIONAL_APPLICATION_REPORTING.md)
adds a separate optional report of exact persisted remote regional facts, with
explicit two-sided regional-only capability negotiation and database session/
receipt fencing. It adds no aggregate application support, cutover caller, report
ACK/cursor, API/UI fields or send/suppression authority. Check its latest named
real-engine evidence and remaining acceptance limits before proceeding.
The [delivery preparation continuation](M7_DELIVERY_PREPARATION.md) reuses durable
commands for exact remote request preparation. It leaves effective regional mode
unchanged and supplies no application support, suppression or cutover authority;
the [protected local preparation continuation](M7_LOCAL_DELIVERY_PREPARATION.md)
adds fresh-activation request persistence and cold refresh upgrades without adding
an execution gate. The [shared request-round continuation](M7_PREPARATION_ROUNDS.md)
adds all-member, DB-clock-expiring request identity and terminal rearm without changing
mode execution. The [durable standby planner](M7_AGGREGATE_STANDBY.md) now reuses
the pure paging evaluator for DB-fenced incident identities and held channel plans
only. Held plans are separate from delivery claims and cannot be promoted into sends.
The new [operative cutover implementation](M7_OPERATIVE_CUTOVER.md) now implements
the committed all-source barrier/application/provider-I/O lifecycle behind the
explicit default-false opt-in. Its strict full race gate passed 5,976 named tests
(570 MariaDB passes; all 181 M7 leaves; zero engine skips). The earlier descriptions
above retain their historical slice scope. No live
aggregate runtime has been activated. A document receipt is still not an
applied-mode receipt, and standby work cannot be promoted into operative sends.

**M5 is complete. Start with [completion and acceptance](M5_COMPLETION.md), then
choose authorized M6 validation/activation work.**
The first M6 slice is the [fleet assignment-ownership gate](M6_FLEET_ACTIVATION_GATE.md):
sharded hub workers attest the assignment protocol they enforce before claiming
any lease, and a desired set with a remote member is refused with
`409 worker_fleet_unaware` while a live lease holder has not attested. Read its
detection boundary before relying on it — local-mode workers are not enumerable
and the check is point-in-time at write. The
[operator requirements](M6_OPERATOR_REQUIREMENTS.md) record the egress,
durability, clock, disk, backup and provider bounds the code enforces. A
[synthetic populated MariaDB rehearsal](M6_PARTITIONED_MIGRATION_REHEARSAL.md)
measures migration 037 on 100k partitioned heartbeats. A
[Go-runner follow-up](M6_MIGRATION_RUNNER_REHEARSAL.md) applies 038–074 to that
populated schema. A [published-release upgrade rehearsal](M6_VALIDATION_REPORT_2026-10-04.md)
starts the actual `v0.4.5` image, seeds 100,000 old partitioned rows and
upgrades them through the working-tree application (34 → 74 migrations); it
still does not close the production-sized disk/lock gate. The
[fresh dual-engine race gate](M6_VALIDATION_REPORT_2026-10-04.md) audits named MariaDB
passes and skips on a disposable database; it is not a scenario-by-scenario
matrix. The [real edge ENOSPC test](M6_VALIDATION_REPORT_2026-10-04.md) covers a T20 recording/health
slice; [abrupt edge-process crashes](M6_VALIDATION_REPORT_2026-10-04.md) cover the T06
local-transaction kill boundary on an isolated tmpfs. Neither is the full
40-case matrix. None of these records
authorizes a canary. The remaining M6 requirements are listed in
[the plan](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation).
The earlier [foundation and frozen read contracts](M5_FOUNDATION.md) remain useful.
The first slice exposes scoped health and desired assignments. The second,
[fleet diagnostics](M5_FLEET_DIAGNOSTICS.md), adds the safe runtime diagnostic
read model and the admin `GET /api/probes` list/detail reads. The third,
[assignment writes](M5_ASSIGNMENT_WRITES.md), adds revisioned admin desired-set
replacement, atomic create-with-assignments and the clone rule. The fifth,
[administrative operations](M5_ADMIN_OPERATIONS.md), freezes the registration
network trust and lands registration writes plus durable enroll/rotate/reset
operations. The sixth,
[regional history](M5_REGIONAL_HISTORY.md), adds the relationship-checked
regional history/chart routes. [M5 completion](M5_COMPLETION.md) adds durable
revocation/delete, fleet and regional UI, incident ACK, public overall coverage,
and authorized browser events, with actual SQLite/MariaDB and process evidence.

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

## M8 continuation

The first [verified SSH preflight slice](M8_SSH_PREFLIGHT.md) is implemented:
`phoenix-probe-admin inspect-host` performs host-key-verified, policy-bound,
bounded nonmutating discovery without loading hub configuration or touching the
hub DB. This is compatibility evidence only, not an install/enrollment/config
receipt. The follow-on [asynchronous preflight API](M8_ASYNC_PREFLIGHT.md) is
implemented as default-disabled admin-only discovery with durable receipts,
API-local bounded execution, immutable retries and joined cancellation. Read
its verification record and credential-loss/expiry contracts before extending it.
The M8-specific real-engine control passed, but broad dual-engine acceptance is
blocked by operative M7 failures; the first was also reproduced at the committed
baseline. Do not infer a green whole-backend MariaDB gate or change those fences inside
this M8 slice. See the failed-run/baseline evidence and limits in that record.
The [pinned binary seam](M8_PINNED_BINARY_ARTIFACTS.md) now prepares a private
verified release-binary snapshot or verifies/disposes it with the offline
`verify-artifact` CLI. It does not independently extract runtime version from
trimmed Go metadata; version-to-digest trust is an explicit operator pin.
The [Docker metadata seam](M8_DIGEST_PINNED_IMAGES.md) now selects a digest-bound
platform reference from an explicit trusted index plus manifest/config bytes.
`verify-image` is offline and DB-independent; config-label provenance is not
runtime-version proof and layer bytes are not verified. Before an installer uses
this seam, verify correspondence with a real built image (including inherited
base config), then independently verify actual digest/platform pull results.
The image metadata checkpoint is committed locally as `54fdc44` (not pushed).
The next [internal install-intent/resource journal](M8_INSTALL_JOURNAL.md) now
records immutable intent, one-shot committed action reports and conservative
expiry/cancellation on both engines. It has no executor, startup or endpoint
wiring; resources_ready is nonterminal, not installed success. Requested artifact
pins and owned flags are not verification or cleanup authority. Use its dedicated
34-leaf MariaDB gate and read its final evidence before continuing. This checkpoint
is committed locally as `7b38761`, not pushed.
The next [one-shot resource coordinator](M8_INSTALL_RUNNER.md) connects acknowledged
Begin/reservation grants to joined executor reports, bounds cancellation and
observes durable revocation. Its dedicated gate requires all 34 journal plus 11
runner MariaDB leaves. It remains an internal use case with an executor port,
not a concrete SSH/Docker/systemd adapter, startup worker or installer endpoint.
Actual artifact delivery, host-side ownership/fencing, the idempotent
Docker/systemd executor, bounded redacted progress, authenticated enrollment
plus exact config activation remain open.
Do not reuse a preflight receipt as installation or cleanup authority. M6
operational/release and deployment approval gates remain unchanged.

## Start here

1. Read [AGENTS.md](../../AGENTS.md), [testing](../TESTING.md), this status, and the
   relevant [architecture](ARCHITECTURE.md) and [protocol](PROTOCOL.md) sections.
2. Inspect HEAD, the working tree and current migrations. Preserve unrelated work;
   do not recreate completed types, enrollment, replay or recovery mechanisms.
3. Select a bounded requirement from [M6](IMPLEMENTATION_PLAN.md#9-m6--failure-validation-and-v1-activation)
   under the user's requested scope. Production activation needs the operator's
   approval; M5 acceptance does not authorize deployment.
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
| Production-process verification | External campaigns described in the [testing guide](../TESTING.md#11-reproduce-validation-and-create-external-uatload-tests) |

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

Further work must preserve M4 compatibility, M5 UI, local-only defaults, current authorization,
JSON names and the locked monitor/provider inventory. Complete design tables may
include future behavior; the status and acceptance records define what runs today.
Historical checkpoint evidence remains committed. Superseded delegation and progress
notes are handled by [documentation maintenance](DOCUMENTATION.md).
