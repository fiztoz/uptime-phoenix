# M3 Ordered Replay Engineering Retrospective

## Summary
The M3 replay increment connects edge telemetry outbox storage to authenticated, fenced hub ingestion across HTTP/TCP/DNS runtimes on branch `codex/multi-region-probe-plan` (baseline `d3eea61`, accepted implementation `f1095f8`). Antigravity supplied initial core drafts and two bounded edge storage/session slices. Codex replaced the authorization draft, implemented hub ingestion and wiring, reproduced defects, and executed the acceptance checks. These were pre-commit integration defects; no deployment incident is claimed.

## Root Cause Analysis
- **Interface Drift & Invented APIs**: Early drafts modified `ProbeConnectionTransport.Run` prior to hub implementation and hallucinated methods (`store.RecordTelemetry`, `Diagnostics.OutboxCount`), causing compilation failure.
- **Fixture Divergence**: Runtime fixtures supplied inconsistent health (`Ready: true` at `config_revision: 0`), omitting identity fingerprints, and skewing timestamps, producing TLS EOF failures.
- **Blocking Backoff & De-synchronized ACK Loop**: In [`internal/adapters/probe/edge_replay.go`](../../internal/adapters/probe/edge_replay.go), `processEvent` slept synchronously on retry, starving queued ACKs. Duplicate ACKs triggered immediate resends (`SendReplay`).
- **Timestamp Precision Mismatch**: Both hub repository adapters persist these lifecycle timestamps at microsecond precision. Persisted `StartedAt` vs wire nanoseconds caused false `transition_identity_conflict` rejections in [`internal/adapters/repository/probe_replay.go`](../../internal/adapters/repository/probe_replay.go). Comparing wire nanoseconds against private edge `observed_at` microseconds falsely failed event row validation.
- **Unscoped Authorization**: Initial authorization drafts checked history without monitor scoping, omitted exact retained config proofs, and treated `channel_version <= config_version` as authority.

## Fixes and Architectural Resolutions
- **Non-Blocking Scheduled Loop**: Replaced blocking sleep in [`edge_replay.go`](../../internal/adapters/probe/edge_replay.go) with a single scheduled-send loop. Readiness gates sends while incoming ACKs process concurrently via a bounded feedback queue (joined on session exit). Exact fence + sent-batch ACK pruning prevents state leakage. Validated range and bounded duration before multiplication; retry cursors describe already durable duplicate prefixes without advancing edge cursors.
- **Microsecond Normalization**: Lifecycle timestamps are normalized to UTC microseconds prior to authority checks and DB writes in [`probe_replay.go`](../../internal/adapters/repository/probe_replay.go); event comparison uses `UnixMicro` without mutating stored event bytes, while immutable full wire digests verify duplicate identity.
- **Transaction-Scoped Authorization**: In [`internal/core/services/probe_replay_authorization.go`](../../internal/core/services/probe_replay_authorization.go), `AccessService.AuthorizeEvent` receives non-secret facts captured inside the write transaction scoped to the specific monitor, enforcing exact retained config membership, assignment intervals and parent channel versions. Hub mirrors never generate provider work.

## How Defects Were Found and Why They Slipped
- **Synthetic Test Assertions**: Early tests mirrored state flags instead of invoking production entry points (`processEvent`). Independent repros [`TestReplayAcceptanceDuplicateACKDoesNotResendCurrent`](../../internal/adapters/probe/replay_acceptance_test.go) and [`TestReplayAcceptanceACKRemainsResponsiveDuringRetryDelay`](../../internal/adapters/probe/replay_acceptance_test.go) exposed loop starvation and resend storms.
- **Precision gap in fixtures**: The initial replay fixtures already truncated timestamps, masking the boundary on both engines. Codex's deterministic repro `testReplayPrecision` in [`internal/adapters/repository/probe_replay_acceptance_test.go`](../../internal/adapters/repository/probe_replay_acceptance_test.go) failed first on SQLite and then passed on both SQLite and MariaDB after the fix.
- **Permission & Session Boundaries**: Headless CLI execution cannot prompt for permissions. Authoring tests with file tools does not execute them; unrun assumptions masked compilation and fixture errors.

## Verification and Validation Evidence
- **Codex Execution Evidence (Not Run by Antigravity)**:
  - `TestProbeReplayAcceptance` passed on BOTH MariaDB 11.8 and SQLite with `-race`: verified mixed receipts, rejections + duplicate prefix, late cursor rollback, zero provider writes, historical generation not current, future evidence not current, expired/stale lease fencing, exact config parent channel, guarded downgrade, concurrent duplicates, lease expiration during batch, and nanosecond identity.
  - Real TLS lost-ACK reconnect passed with race detection: validated higher hub welcome cursor, retained local outbox, and exact byte replay.
  - Storage fault injection passed: exact bytes, bounds, sequence holes, `MaxInt64`, fence enforcement, late cursor/delete rollback, and reopen.
  - Static checks: `golangci-lint` reported 0 issues; CGO-free binary builds passed.
- **Integrator status**: The two-worker process smoke passed with hub and edge cursors at 69, 22 offline events replayed, one resolved incident, two successful notifications and zero hub send intents. `make gate-full` passed with exit 0, and the complete live MariaDB repository matrix passed in 201.407s; see [the acceptance record](M3_REPLAY_ACCEPTANCE.md). M3 retention eviction, state recovery, watchdogs, commands, and fleet UI remain incomplete.

## Concrete Rules for Next Assignment
1. **Freeze Contracts & Disjoint Ownership**: Agree on wire/domain DTOs before coding; never edit outside assigned paths ([`docs/multi-region/M3_REPLAY_WORK_CONTRACT.md`](M3_REPLAY_WORK_CONTRACT.md)).
2. **Inspect Real APIs First**: Never invent methods or assume struct fields; verify concrete signatures in source before drafting callers or mocks.
3. **Single Bounded Goals**: Focus on one component per turn; avoid broad sweeps that wander into guessed paths.
4. **Test Production Paths, Not Conditions**: Drive tests through real handlers (`processEvent`, `Check`); never duplicate internal branching logic in tests. Keep validation strict—fix fixtures, never relax production checks.
5. **Trace Engine & Clock Boundaries**: Normalize repository time inputs to UTC; normalize replay incident identity to the documented microsecond storage precision; account for MariaDB vs SQLite differences; order by `(time, id)` for deterministic tie-breaking.
6. **Report Authored vs. Run Honestly**: Antigravity authors files; tests authored with file tools are **not run by Antigravity**. Never claim milestone completion or infer passing status from unexecuted code.
