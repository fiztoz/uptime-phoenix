# Multi-region continuation guide

M3 is complete for HTTP/TCP/DNS at implementation `a12a3fa`, recorded in `8b455d4`.
Read [current status](IMPLEMENTATION_STATUS.md) and [final acceptance](M3_COMPLETION_ACCEPTANCE.md)
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
| Runtime use cases | `internal/core/services/probe_*`, `internal/core/ports/` |
| Wire, TLS and sessions | `internal/adapters/probe/` |
| Hub authority and history | `internal/adapters/repository/probe_*` |
| Edge persistence and retention | `internal/adapters/repository/edge/` |
| Production-process verification | `scripts/probe_runtime_smoke.py`, [testing guide](../TESTING.md) |

The [operator guide](M2_OPERATOR_GUIDE.md) covers manual enrollment, configuration,
watchdogs, ACK and rotation. [Reset acceptance](M3_HUB_RESET_ACCEPTANCE.md) records
the explicit recovery workflow; [key provisioning](KEY_PROVISIONING.md) covers
installation-key handling. Keep source identity and archived evidence intact.

## Constraints carried forward

Database authority, current-state transfer, historical ingest and provider sends
are separate effects. Configuration/application receipts follow commit. Queued
history must not create remote provider work on the hub. Regional ACKs identify
their original incident. Cleanup preserves unresolved dependencies and tombstones.
No lease lock spans provider I/O. See [the retrospective](../postmortems/2026-09-21-m3-integration.md)
for the reproduced failures behind these rules.

M4 compatibility and M5 UI must preserve local-only defaults, current authorization,
JSON names and the locked monitor/provider inventory. Complete design tables may
include future behavior; the status and acceptance records define what runs today.
Historical checkpoint evidence remains committed. Superseded delegation and progress
notes are handled by [documentation maintenance](DOCUMENTATION.md).
