# Multi-region probes

Status: M0–M3 engineering acceptance is complete for the HTTP/TCP/DNS remote runtime. M4 compatibility, M5 fleet UI and M6 release validation remain. See [current status](IMPLEMENTATION_STATUS.md) and [M3 acceptance](M3_COMPLETION_ACCEPTANCE.md). No production rollout is implied.

Prepared: 2026-09-13. Application baseline: `main` at `5183093c5c218675bab89fe9d6c7056de2d711eb`. Research baseline: Gemini's `research_multi_region_deployment` at `f4ef4a4677120264cb513158df813479ee5dd644`.

## Goal

Let one private Phoenix hub manage independent execution probes, assign each monitor to one or more probes, collect regional history, and keep probe checks and direct notifications operating when the hub or connecting network is unavailable.

## Read in this order

1. [Project instructions](../../AGENTS.md), [project architecture](../ARCHITECTURE.md), and [testing guide](../TESTING.md).
2. [Continuation guide](CONTINUATION_GUIDE.md) and [current implementation status](IMPLEMENTATION_STATUS.md): concrete first assignment, code map, pitfalls, acceptance tests, and handoff instructions for continuing agents.
3. [Architecture and decisions](ARCHITECTURE.md): ownership, state machines, persistence, trust, compatibility, deployment, and failure behavior.
4. [Protocol and API contract](PROTOCOL.md): transport messages, delivery semantics, administrative APIs, browser events, and exact field names.
5. [Implementation plan](IMPLEMENTATION_PLAN.md): dependency-ordered milestones, file ownership, verification matrix, rollout, and agent handoff.
6. [Original Gemini research](../../research/distributed-agent-worker-az-architecture.md): historical rationale and illustrations. Its executable-looking examples are not implementation contracts.

The implemented standalone key tool is documented in
[key provisioning and recovery](KEY_PROVISIONING.md).

These documents define the proposed feature together. Existing `AGENTS.md` rules remain authoritative. Update the contract before implementing any intentional departure, and record the reason in the decision log below. They do not mark the feature complete or authorize a production deployment.

## Delivery boundaries

| Delivery | Included |
|---|---|
| Engineering vertical slice | One hub, one remote probe, manual enrollment, HTTP/TCP/DNS checks, durable recording and replay, independent regional alerts, connection watchdog |
| V1 release | Full supported pull-checker capability coverage, many-to-many assignments, scoped state, regional conditions/certificates, maintenance and alert lifecycle, UI, migrations, upgrade/restore handling, failure tests |
| Follow-on milestones | Optional consolidated monitor alerts; verified SSH provisioning; optional public push gateway |
| Separate future project | Automatic hub/database failover across regions, a public dashboard surviving hub loss, multi-tenant management, arbitrary remote command execution |

The default single-pod installation must continue to work without probes, Redis, a public IP, or any newly required external service. No monitor type, notification provider, role, or capability flag is added by this design.

## Decision log

| ID | Decision | Reason |
|---|---|---|
| D01 | One authoritative logical hub; private hub dials public probes | Meets private-K8s networking requirements without inbound hub exposure |
| D02 | A logical probe is a vantage point; a worker is a replaceable executor | Worker sharding and geographical redundancy solve different problems |
| D03 | Each probe owns regional retries, incidents, and direct delivery in connected and disconnected states | Preserves independent alerting and avoids delivery ownership changing at a network partition |
| D04 | Aggregate health is computed independently; aggregate paging is a later opt-in | Keeps regional evidence separate from policy and avoids unexpected duplicate pages |
| D05 | Durable at-least-once telemetry with idempotent hub ingest | Survives lost acknowledgements and restarts without inflating history |
| D06 | Pinned TLS plus scoped, rotatable credentials for V1 | Reuses existing Go dependencies; requires verified initial trust and rejects empty pins |
| D07 | Explicit `UNKNOWN` health and coverage | Missing regional evidence must not look healthy or be silently counted as downtime |
| D08 | Hub DB persistence and live projection are separate from edge evaluation | Replayed events must not rerun retry logic or send historical alerts |
| D09 | Manual enrollment precedes optional SSH installation | Allows the reliability path to be validated without coupling it to host provisioning |
| D10 | The reserved local probe ID is `local` | It works for Kubernetes, a standalone server, and local development |
| D11 | New branch starts from current local `main`, retaining the research text with trailing whitespace normalized | Carries current application improvements and makes research available to the next agent |
| D12 | Raw checker conditions and evaluated current conditions use different DTOs | A first warning is not yet a confirmed warning; replay must not repeat promotion |
| D13 | Remote acknowledgement URLs deferred from V1; local opaque-token URLs unchanged | Offline source incidents have no defined hub token authority; authenticated commands provide scoped acknowledgement |
| D14 | Coverage excludes maintenance and counts PENDING/paused as unknown | Prevents missing evidence or administrative pause from inflating uptime |
| D15 | Dependency versions use the complete snapshot revision | Existing timestamps are not reliable configuration version counters |
| D16 | State snapshots carry explicit observed, candidate, and effective conditions; transfer completion is separate from durable application | Preserve first-sample and hysteresis behavior during backlog replay without inferring alerts or advancing the history cursor |
| D17 | Incident subject identities are immutable; deliveries reference the source incident transition and keep provider event names | Regional replay can mirror lifecycle and outcomes without becoming a send request or claiming hub-owned aggregate/group incidents |
| D18 | Handshake identity/generation/capabilities are checked against trusted expectations; empty retained history uses sequence zero and health is role-specific diagnostic evidence | Prevents silent reset, cursor jumps, fabricated queue health, and capability/version substitution before runtime integration |
| D19 | Config snapshots use explicit bounded dependency graphs, exact capability unions, and symmetric expanded maintenance links; assembly grants no activation receipt | Preserves existing target visibility, templates, minute-based resend/escalation, and disabled-policy semantics while rejecting incomplete or misdirected configuration |
| D20 | Persist complete membership/policy revisions with half-open UTC ranges; backfill only the latest known revision and count missing historical membership as UNKNOWN | Removal, re-addition, and policy edits must not rewrite past uptime or silently exclude gaps from coverage |
| D21 | Capacity and certificate state use probe plus assignment generation; legacy evidence backfills to local generation one and compatibility reads select only the current local assignment | Re-added probes cannot inherit old promotion/notification cursors, and remote evidence cannot replace a legacy local dashboard value |
| D22 | Allocate one durable local-stream sequence with atomic heartbeat/observation/state recording; retry stale state evaluations without changing assignment identity | Independent monitors cannot reuse sequences, and failed writes cannot leave a heartbeat or consume a sequence |
| D23 | Persist availability attempt throttles by monitor/probe/generation; reserve resends atomically before provider I/O and keep the legacy dispatcher local-only | Restart and worker handoff retain backoff; one region or generation cannot consume or clear another's cursor. Incident/outbox integration remains required for durable delivery |
| D24 | Availability incidents use monitor/probe/assignment generation; escalation inherits that immutable identity through its alert ID | Acknowledgement, recovery, and a re-added assignment cannot reuse another incident. Legacy HTTP views remain local-only; the hub runner never claims remote ladders and cancels obsolete local generations before delivery |
| D25 | Source delivery intents are separate from mirrored outcomes, committed with the observation and incident, and completed through expiring attempt leases | A queued identity survives restart; a stale worker cannot overwrite a newer attempt. The storage API captures availability context without provider secrets and grants no sending authority. Live dispatcher activation waits for versioned channel configuration, lifecycle planning, and obsolete-intent reconciliation |
| D26 | Legacy alerts retain their API IDs and secret ack tokens while gaining unique source UUIDs and atomic lifecycle versions | Source lookup remains assignment-scoped. Publication must join the recorder transaction; referenced source mappings block downgrade, while unpublished mappings may reset on explicit rollback |
| D27 | Prepared configuration preserves exact bytes as authenticated ciphertext in immutable per-probe revisions; preparation does not activate configuration | Revision conflicts cannot replace retained credentials. Local-only decoding preserves push, direct Docker configuration and ack links without relaxing remote V1. Key provisioning, runtime validation, activation and consumer reconciliation remain explicit gates |
| D28 | Local snapshot construction reads one committed database view, preserves exact maintenance links, and filters to the selected dependency graph | MariaDB uses explicit repeatable-read isolation; SQLite keeps one read transaction. Unlinked maintenance suppresses nothing, matching the live service. Prepared source content is not a current-configuration fence or an activation receipt |
| D29 | Provision installation keys explicitly with atomic no-replace publication; loading never creates or repairs a key | Missing keys must not silently orphan retained ciphertext. File-only validation grants no database readiness or activation authority; runtime wiring must authenticate retained snapshots |

## Current continuation

Use [the continuation guide](CONTINUATION_GUIDE.md) for the current implementation
map and next milestone boundaries. The original research and decision log remain
design context; old agent assignments are not active. [Documentation maintenance](DOCUMENTATION.md)
explains retained acceptance evidence, the local archive and deleted drafts.
