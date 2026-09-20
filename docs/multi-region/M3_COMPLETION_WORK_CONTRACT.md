# M3 completion work contract

Baseline: `eb5c59e`, 2026-09-20. User objective: continue until M3 is complete.
The complete milestone in `IMPLEMENTATION_PLAN.md` section 6 remains the goal.
No push or deployment is authorized. Preserve the existing local commits.

## Completion requirements

1. Complete config snapshots, atomic application and durable receipts (verify existing implementation).
2. Ordered replay, atomic cursor/receipts, explicit retryable errors, durable retention gaps and bounded disk use.
3. High-priority current-state snapshots while backlog drains; no historical cursor advance or regional provider work.
4. Per-assignment time/sequence guards and dirty historical buckets recomputed through every resolution and overall history.
5. Both connection watchdogs: application ingest health, startup arming, durable incident identity, 90-second loss and 30-second stable recovery.
6. Durable idempotent commands, offline incident-specific acknowledgement, credential and certificate rotation, and explicit stream-reset recovery.
7. Queue pressure diagnostics and graceful shutdown with bounded flushing.
8. Real 15-minute link partition with target failure/recovery, edge restart and retained-history replay exactly once; fresh health ahead of backlog and a pending acknowledgement applied exactly once to its original incident.
9. Full repository gate, real SQLite/MariaDB contracts, no new dependencies or monitor/provider types, coherent conventional local commits and current operator/status evidence.

This is a requirements list, not a completion claim. Completion requires current-state evidence for each item, including failure and restart paths. Fleet UI and M4 compatibility work are outside M3 except where a milestone invariant depends on them.

## Ownership

Codex owns all production source, tests, migrations, shared contracts and documentation, execution, verification and commits. Antigravity is assigned a read-only M3 requirement audit and may write only `docs/multi-region/M3_COMPLETION_REQUIREMENTS_AUDIT.md`. It must not edit source or claim tests ran. Further bounded ownership requires a new explicit assignment.

## Running evidence ledger

- Initial retry policy unit tests passed. Authority failures and unreadable/rolled-back cursors do not produce successful ACKs.
- First focused race run: edge retention, probe replay and core tests passed; hub gap insertion failed on both SQLite and MariaDB. A test-only query hook showed Bun inferred `affected_monitor_i_ds` from `AffectedMonitorIDs`. The schema uses `affected_monitor_ids`; an explicit column tag fixes that mapping. This was a Codex integration defect, not an Antigravity implementation claim. Query instrumentation was removed; both engines passed the race-enabled gap acceptance rerun (4.310s).
- Antigravity completed a baseline requirements audit, then corrected invented retention thresholds and unsafe reset suggestions. Its `/learn` workflow also edited `AGENTS.md` outside the report-only assignment. Codex verified the tool log and reverted only that added section; future delegation must enforce file ownership even for workflow commands. No canonical project-rule change is accepted from that run.

## Bounded transport-test assignment

The requirements audit is finished and its ownership is returned. Antigravity may now edit only `internal/adapters/probe/hub_replay_recovery_test.go`, a new transport acceptance test. It must not edit `AGENTS.md`, any other test/source, shared docs, settings or skills. The frozen runtime callback remains `HubTransport.Run(..., ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error))`; the domain batch has either ordinary events or `Gap *domain.ProbeTelemetryGap`. `domain.ErrReplayRetry` plus a result with durable cursor produces `telemetry.retry`. All other errors close. Codex runs the tests and retains integration ownership.

- Antigravity's transport test passed the gap and retry cases. Its extra establishment-failure case assumed queued config frames could not precede revocation. Codex reproduced that failure and corrected the assertion to forbid successful replay/health receipts while allowing already queued config frames. The requested Antigravity follow-up could not write in headless mode; Codex completed the correction within integrator ownership. No permission settings were weakened.
- Edge retention tests now include bounded fragmentation (1,025 disjoint loss ranges), a 1 MiB delivery reservation budget and progress after all reserved outcomes commit. The focused race run passed (4.150s).
- All Antigravity file ownership is returned. No delegated process may resume edits without a new explicit assignment.

## Next source contract: high-priority current state

- Edge builds one coherent source-state view of its current accepted config and high-water sequence. The latest source evidence must survive replay pruning. Snapshot encoding uses the existing explicit state DTOs; unsupported condition/TLS state is rejected rather than silently dropped.
- State transfer uses the control queue with an independent bounded receipt deadline. Initial state is applied before the first backlog batch; periodic state stays ahead of subsequent replay frames. State receipts never call `CommitReplayACK`.
- Hub application checks the current DB lease, installation key, active stream and exact applied configuration/assignment authority. It persists the receipt and all current-state/missing-assignment effects atomically. It creates no observations, provider intents or historical cursor changes.
- Existing newer current evidence survives older sequence or assignment generation. Sequence ordering remains authoritative across a backward wall-clock step; unreasonable future evidence remains ineligible for live projection. Missing assigned entries clear stale current evidence with a persisted source-watermark barrier so later old replay cannot restore it. Fresh observation sequences beyond the barrier can restore evidence.
- Transfer interruptions, lost receipts, restart, stale sessions, config changes and late transaction failures require real SQLite/MariaDB and transport effect assertions.
- This contract does not declare the state slice implemented. Codex retains production ownership.


## Read-only state review disposition

Antigravity reviewed the proposed state slice in conversation `d7182e03-c845-4319-a2ad-beb4c1cac68a`; it edited no repository files. Its report is advisory, not acceptance evidence.

- Accept the non-overlapping transfer/receipt deadline requirement. One sender waits for its matching durable receipt before starting another snapshot; a 15-second ticker must not create concurrent staging. Health must retain bounded latency.
- Accept the backward-clock finding after tracing `ARCHITECTURE.md` section 4.3 and acceptance T21. Sequence governs same-probe current state even when wall clocks move backward. The existing `updateReplayState` comparison of observation timestamps incorrectly rejects a later sequence after a clock correction; the state increment must fix it and add a regression. Rejecting unreasonable future evidence and deriving freshness remain separate rules. Codex initially misread the contract and corrected this disposition before implementing the slice.
- Preserve TLS scope honestly. Current edge config rejects certificate paging and the availability encoder emits null TLS. That does not make HTTPS availability invalid. Current-state support must either mirror a supported auxiliary payload or reject it explicitly; silently discarding a supplied payload is unacceptable. Extending source TLS/condition support must be separately traced before claiming it works.
- Reject restoring an omitted current assignment from earlier historical replay. A coherent snapshot at stream watermark 100 already accounts for events through 100. Allowing replayed sequence 95 to undo its missing-state reconciliation would contradict snapshot completeness. The barrier is scoped to the same monitor, assignment generation and stream; later genuine evidence may restore it.
- Frame dispatch is part of the implementation contract and will be tested through `HubTransport.Run`; its current absence is known baseline work, not a newly discovered flaw in the proposed design.

- Retention checkpoint gates passed: CGO-free build, lint with zero issues, final full Go race suite (handler package 365.176s), complete live MariaDB/SQLite repository matrix (194.982s), and diff whitespace checks. The next state files are excluded from this checkpoint and are not accepted yet.
