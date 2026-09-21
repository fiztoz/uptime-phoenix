# M3 source storage bounds acceptance

Date: 2026-09-21. Baseline `728aefd`. This checkpoint is accepted. The actual
900-second partition and final audit are accepted in the [whole-M3 record](M3_COMPLETION_ACCEPTANCE.md). Codex owns verification.
See [hashed evidence](M3_STORAGE_BOUNDS_EVIDENCE.json),
[retrospective](M3_STORAGE_BOUNDS_RETROSPECTIVE.md) and
[contract](M3_STORAGE_BOUNDS_WORK_CONTRACT.md).

Migration 010 adds a separate 64 MiB metadata quota and transactional bounded
retirement. Current configuration/state, unresolved incidents and pending/leased
deliveries survive. Old eligible config, resolved incident and terminal delivery
history retire after the greater of seven days and the configured telemetry
horizon. Retained assignment generations still reject reuse after their old
configuration is gone; guarded downgrade refuses an invalid old foreign key.
Diagnostics expose `state_pressure` at 80%. Serialized telemetry and command
receipts keep their independent retention rules.

The database page budget is max(1 GiB, twice telemetry capacity plus 512 MiB).
Existing larger databases can reuse/delete their pages without further growth.
At 16 MiB of WAL, the next writer must checkpoint before admission; a pinned reader
blocks new writes until it releases. This threshold allows one admitted
transaction's additional frames and is not a filesystem quota. The store reserves
its sole connection across the check and transaction and reapplies the page cap
after driver reconnects. No forced reader termination or VACUUM is required.

Real SQLite effect tests passed exact quota rollback/recovery, populated migration
round trips and refusal, oversized upgrade cleanup, seven-day minimum retention,
512-row progress, pending/leased work preservation, unchanged queued bytes and
late-failure rollback. Repeated fill/drain cycles stabilized at 33,816,576 database
bytes and 9,002,232 WAL bytes. A real independent reader proved that further WAL
growth stops and admission recovers after releasing the read transaction.

Final CGO-free build, full race suite and lint passed with unchanged source hashes:
3,719 named passes in 22 packages, zero failures, two optional skips, 315
MariaDB-named passes and 303 audited live MariaDB cases. No engine tests skipped.
The initial gate's reset-test clock failure and the separate assignment deadlock
found by the process rehearsal are retained in
[the verification retrospective](M3_ASSIGNMENT_LOCK_RETROSPECTIVE.md).

```text
Commands executed: CGO_ENABLED=0 go build ./...; go test -race -count=1 -timeout=20m -json ./...; golangci-lint run; focused real-engine regressions.
Engines and named tests exercised: real SQLite and disposable MariaDB; TestEdgeMetadata*, TestEdgeStorage*, complete repository gate.
Passed / failed / skipped: 3,719 final named passes; zero final failures; two optional skips; earlier failed reproduction/gate retained separately.
Acceptance criteria still unverified: none for storage. Whole-M3 acceptance is linked above.
```

No dependencies, monitor/provider types, frontend or Helm changes were added.
No push or deployment.
