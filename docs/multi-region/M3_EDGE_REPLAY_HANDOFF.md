# M3 Edge Replay Persistence Slice Handoff

> Historical Antigravity handoff, not acceptance evidence. Codex found and fixed
> compilation, fixture and behavior defects after this draft. Read
> [the retrospective](M3_REPLAY_RETROSPECTIVE.md) and
> [final acceptance](M3_REPLAY_ACCEPTANCE.md) for the validated implementation.


**Date:** 2026-09-20\
**Author:** Antigravity (Gemini 3.8 Flash High)\
**Integrator:** Codex\
**Status:** Complete for bounded persistence slice. All tests **NOT RUN BY ANTIGRAVITY** (Codex owns runtime integration and actual test execution).

---

## 1. Scope & Ownership Summary

This handoff covers exclusively the edge replay persistence slice and its focused SQLite tests:
- `internal/adapters/repository/edge/replay.go` (updated)
- `internal/adapters/repository/edge/replay_test.go` (new)
- `docs/multi-region/M3_EDGE_REPLAY_HANDOFF.md` (this file)

No other files were modified. `store.go`, core domain/port interfaces, transport adapters, and hub ingestion remain owned by Codex. Antigravity executed no shell commands, commits, pushes, database runs, or external planning.

---

## 2. Implemented Corrections & Contracts

### 2.1 Coherent SQLite Transaction & Single-Connection Discipline
- Removed unused imports from `replay.go`.
- `ReadReplayBatch` reads `edge_identity` and `edge_telemetry_outbox` rows within **one coherent SQLite transaction** using `s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error { ... })`.
- Within the transaction, `readIdentity(ctx, tx)` and `tx.NewSelect()` are used directly on `tx`. This avoids recursive calls to `s.ReadIdentity` and respects SQLite's single-connection configuration (`MaxOpenConns(1)`).
- `CommitReplayACK` reuses `s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error { ... })`.

### 2.2 Sequence Progression, Cursor Logic, and Gap Detection
- `fromSeq = 0` resolves to the local committed cursor + 1 (`i.CommittedSeq + 1`).
- Explicit `MaxInt64` exhaustion returns an empty batch (`FirstSeq: math.MaxInt64, LastSeq: math.MaxInt64, Items: nil, TotalBytes: 0`) without signed integer overflow.
- Explicit `fromSeq > 0` must strictly match the next expected sequence (`i.CommittedSeq + 1`). Any attempt to skip unacknowledged pending data or query pruned history fails with `ports.ErrConflict`.
- Missing first, middle, or tail rows when `last_created_seq` proves pending data exists (`last_created_seq > committed_seq`) fail visibly with `ports.ErrConflict` rather than returning an empty healthy queue.
- If a batch limit (`maxEvents` or `maxBytes`) is reached, the valid prefix of contiguous items is returned.

### 2.3 Exact-Byte Preservation and Bound Enforcement
- Exact payload bytes are preserved from SQLite `BLOB` storage to `domain.EdgeReplayItem.Payload`.
- Caller bounds are strictly validated:
  - `maxEvents`: permitted in range `1..256`; outside range returns `domain.ErrValidation`.
  - `maxBytes`: permitted in range `1..512KiB` (524,288 bytes); outside range returns `domain.ErrValidation`.
  - `fromSeq < 0`: returns `domain.ErrValidation`.
- Individual payloads are bounded to 64 KiB (`maxEventBytes = 64 << 10`). Payloads of 0 bytes or > 64 KiB return `ErrStorage`. Single events exceeding requested `maxBytes` return `domain.ErrValidation`.

### 2.4 ACK Fencing, Contiguous Coverage, and Idempotence
- `CommitReplayACK` validates input parameters: `HubID`, `ProbeID`, `StreamID`, positive `ConnectionGeneration`, matching `result.StreamID`, and non-negative `result.CommittedSeq` (`domain.ErrValidation`).
- Inside the transaction, verifies exact match with stored identity: `HubID`, `ProbeID`, `StreamID`, and `ConnectionGeneration` (`ports.ErrConflict`).
- Cursor bounds check: `result.CommittedSeq` cannot move backwards (`< i.CommittedSeq`) or reference uncreated sequences (`> i.LastCreatedSeq`).
- Contiguous sequence coverage is verified before pruning:
  `SELECT COUNT(*) FROM edge_telemetry_outbox WHERE seq > ? AND seq <= ?` must equal `result.CommittedSeq - i.CommittedSeq`. Any missing sequence fails with `ports.ErrConflict`.
- Repeated ACK at the current cursor (`result.CommittedSeq == i.CommittedSeq`) is idempotent (`nil`), but the generation and identity fence checks are never bypassed.
- Signed 64-bit arithmetic does not overflow.

### 2.5 Atomic Cursor Update and Pruning
- Cursor update (`UPDATE edge_identity SET committed_seq = ? WHERE id = 1`) and row deletion (`DELETE FROM edge_telemetry_outbox WHERE seq <= ?`) execute in the same transaction inside `s.write`.
- Late delete failure rolls back the cursor update.
- Late cursor failure preserves outbox rows.
- Unhandled errors are redacted to `ErrStorage` via `storageError(ctx, err)`.
- Pruning strictly targets `edge_telemetry_outbox`. Delivery outbox (`edge_delivery_outbox`), active configuration (`edge_config`), and regional state/alerts (`edge_regional_state`, `edge_alerts`) are untouched.

---

## 3. Authored Test Suite (`replay_test.go`)

The test file `internal/adapters/repository/edge/replay_test.go` provides full SQLite coverage using real temporary directories and migrations:

1. `TestReplayRead_ExactBytePreservationAndPayloads`: Verifies exact binary byte preservation (including null bytes, high bytes, UTF-8, and 64 KiB payload), kind, sequence, and UTC timestamps.
2. `TestReplayRead_CountAndByteBounds`: Tests input validation (`fromSeq`, `maxEvents`, `maxBytes`), count limits (`maxEvents = 4`), byte limits (`maxBytes = 250`), single-event budget overflow, and detection of corrupt/oversized rows.
3. `TestReplayRead_MissingFirstMiddleTail`: Tests empty queue vs missing first row (empty table and non-empty table), missing middle row, missing tail row, valid prefix return on batch limit, skipping unsent data, and reading below committed cursor.
4. `TestReplayRead_OverflowBoundary`: Tests `math.MaxInt64` sequence exhaustion without overflow, and reading an event at `math.MaxInt64`.
5. `TestReplayRead_ConcurrentAppendCoherentReads`: Tests concurrent writer (`CommitEdgeCheck`) and reader (`ReadReplayBatch`) to ensure transactions produce strictly contiguous, coherent batches.
6. `TestReplayACK_FencingAndIdempotence`: Tests input validation, fence mismatches (hub, probe, stream, generation), cursor bounds, ACK across missing sequence, successful ACK, repeated ACK idempotence, and generation fence enforcement on repeated ACK.
7. `TestReplayACK_LateWriteRollbackCases`: Uses SQLite triggers to verify that late delete failure rolls back `committed_seq`, and late cursor failure preserves outbox rows.
8. `TestReplay_ReopenPersistence`: Verifies that committed cursor and remaining outbox rows persist across store close and reopen, resuming from `committed_seq + 1`.

---

## 4. Execution & Handoff Notice

- **Test Execution:** Tests were **NOT RUN BY ANTIGRAVITY** per the execution boundary rules. Codex owns formatting, compilation, test execution, and runtime integration.
- **Next Steps for Codex:**
  1. Run `gofmt` / `golangci-lint` on `internal/adapters/repository/edge/replay.go` and `internal/adapters/repository/edge/replay_test.go`.
  2. Run `go test -v -race ./internal/adapters/repository/edge/...`.
  3. Proceed with runtime transport and hub ingestion integration per `docs/multi-region/M3_REPLAY_WORK_CONTRACT.md`.
