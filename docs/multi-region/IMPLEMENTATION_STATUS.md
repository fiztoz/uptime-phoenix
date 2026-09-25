# Multi-region implementation status

Updated 2026-09-25 after the M4 escalation slice; see
[escalation acceptance](M4_ESCALATION.md). Accepted M3
implementation: `a12a3fa`; completion evidence: `8b455d4`.
This is the current status. Check HEAD and newer acceptance records before starting work.

| Milestone | Status | Evidence / next boundary |
|---|---|---|
| M0 contracts | Accepted engineering baseline | [M2 integrator acceptance](M2_ACCEPTANCE_REPORT.md) includes M0 regressions |
| M1 regional persistence and local parity | Accepted after integration corrections | [M1 retrospective](../postmortems/2026-09-20-m1-integration-followup.md), [M2 gate](M2_ACCEPTANCE_REPORT.md) |
| M2 autonomous runtime and enrollment | Accepted for HTTP/TCP/DNS | [M2 acceptance](M2_ACCEPTANCE_REPORT.md), [operator guide](M2_OPERATOR_GUIDE.md) |
| M3 synchronization and recovery | Complete for the supported runtime | [Final acceptance](M3_COMPLETION_ACCEPTANCE.md), [hashed evidence](M3_COMPLETION_EVIDENCE.json) |
| M4 compatibility | Pull-checker, TLS evidence, certificate paging, capacity state, capacity paging, and escalation slices accepted | [Initial coverage](M4_PULL_CHECKER_ACCEPTANCE.md), [Docker bindings](M4_DOCKER_BINDINGS.md), [TLS evidence](M4_TLS_EVIDENCE.md), [certificate paging](M4_CERT_PAGING.md), [capacity state](M4_CAPACITY_STATE.md), [capacity paging](M4_CAPACITY_PAGING.md), [escalation](M4_ESCALATION.md). Open: group/status-page recovery, Insights, backup and deployment compatibility |
| M5 fleet UI and API | Not complete | Administrative workflows, scoped regional views and browser integration |
| M6 release validation | Not complete | Cross-feature failure tests and controlled V1 activation |
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
stay off. Group/status-page recovery, Insights, backup/deployment compatibility
and fleet UI remain M4/M5. Watchdog ACK is
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
