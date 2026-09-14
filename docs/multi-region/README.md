# Multi-region probes — implementation handoff

Status: implementation started with M0 contracts and a bounded M1 persistence/health foundation. No distributed-probe runtime is enabled. See [implementation status](IMPLEMENTATION_STATUS.md) for completed work and the next steps.

Prepared: 2026-09-13. Application baseline: `main` at `5183093c5c218675bab89fe9d6c7056de2d711eb`. Research baseline: Gemini's `research_multi_region_deployment` at `f4ef4a4677120264cb513158df813479ee5dd644`.

## Goal

Let one private Phoenix hub manage independent execution probes, assign each monitor to one or more probes, collect regional history, and keep probe checks and direct notifications operating when the hub or connecting network is unavailable.

## Read in this order

1. [Project instructions](../../AGENTS.md), [project architecture](../ARCHITECTURE.md), and [testing guide](../TESTING.md).
2. [Architecture and decisions](ARCHITECTURE.md): ownership, state machines, persistence, trust, compatibility, deployment, and failure behavior.
3. [Protocol and API contract](PROTOCOL.md): transport messages, delivery semantics, administrative APIs, browser events, and exact field names.
4. [Implementation plan](IMPLEMENTATION_PLAN.md): dependency-ordered milestones, file ownership, verification matrix, rollout, and agent handoff.
5. [Original Gemini research](../../research/distributed-agent-worker-az-architecture.md): historical rationale and illustrations. Its executable-looking examples are not implementation contracts.

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

## Handoff state

- Implementation is in progress. M0 and M1 remain incomplete until all acceptance criteria pass; the status document identifies the executable subset.
- The branch is intended to be checked out in the ordinary repository directory, not a new linked worktree.
- Continue with remaining milestone M0 contracts and M1 work in the status document. Do not copy the research's Go snippets or SQL directly into production files.
- Commit each coherent implementation milestone with its tests. Keep defaults compatible until the explicit activation gate passes.
- Multiple agents may implement disjoint milestones after shared contracts land; the ownership table assigns every shared integration surface to one integrator.
