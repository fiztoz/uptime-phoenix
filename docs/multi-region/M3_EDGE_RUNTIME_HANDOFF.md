# M3 Edge Runtime Session Replay Pump — Handoff

> Historical Antigravity handoff, not acceptance evidence. Codex found and fixed
> compilation, fixture and behavior defects after this draft. Read
> [the retrospective](M3_REPLAY_RETROSPECTIVE.md) and
> [final acceptance](M3_REPLAY_ACCEPTANCE.md) for the validated implementation.


**Date:** 2026-09-20\
**Agent:** Antigravity (Gemini 3.8 Flash High)\
**Deliverable:** Bounded Edge session replay pump and test suite. Does not complete all M3.

---

## 1. Overview & Deliverable Summary

This slice implements the durable edge telemetry outbox replay pump over the established probe WebSocket session:
- Connects the edge telemetry outbox (`ports.EdgeReplayRepository`) to authenticated, session-fenced transmission using `Session.SendReplay`.
- Preserves backwards compatibility on `EdgeRuntime` via `SetReplayRepository(repo ports.EdgeReplayRepository)`. When nil, existing health/config-only behavior is untouched.
- Enforces strict readiness gating, exact byte preservation, single in-flight batch tracking, deterministic ACK validation, proven duplicate ACK filtering, and bounded backoff.

---

## 2. Review Corrections & Refinements

Following compiler and review feedback from Codex, the following corrections have been applied:
1. **Unused Imports Cleaned:**
   - Removed unused `encoding/json` from `edge_replay.go`.
   - Removed unused `errors` and `ports` from `edge_replay_test.go`.
2. **Real API Usage in Fixtures:**
   - Replaced non-existent `store.RecordTelemetry` and `diag.OutboxCount` calls in tests with direct SQLite database operations (`sql.Open("sqlite", ...)`) matching the established pattern in `replay_acceptance_test.go`.
3. **Retransmission Readiness & Non-Blocking ACK Processing:**
   - In `sendAndAwaitACK`, pending retransmissions pause if the hub is unready (`!p.isHubReady()`).
   - Crucially, while paused for readiness or backoff, the pump continues to process incoming frames on `eventCh`, ensuring a durable ACK can still commit even if the hub reports unready immediately after acknowledging.
4. **Proven Duplicate ACK Handling:**
   - Duplicate ACKs for previously committed batches may report shifts between `accepted_count` and `duplicate_count` on retransmission.
   - A proven duplicate is accepted if: stream matches, generation matches, committed cursor matches, rejection identities match, and total coverage (`accepted + duplicate + len(rejected) == totalEvents`) matches.
   - Duplicate ACKs never prune the outbox again and do not immediately trigger a batch resend.
5. **Retry Cursor Validation & Overflow Protection:**
   - Validates that `telemetry.retry` carries a `committed_seq` that never exceeds `inflight.lastSeq` and never advances beyond the local committed cursor.
   - Clamps `RetryAfterMS` to `[100, 30000]` *before* multiplying by `time.Millisecond` to prevent `int64` duration overflow.
6. **Strict `buildReplayBatchFrame` Validation:**
   - Validates `batch != nil`, `streamID == batch.StreamID`, `gen > 0`, event count `1..256`, contiguous sequence numbers (`item.Seq == FirstSeq + i`), individual payload size (`<= 64 KiB`), and total frame size (`<= MaxBatchBytes`).
7. **Microsecond Timestamp Comparison:**
   - Payload JSON timestamps carry nanosecond precision, whereas the SQLite edge outbox stores microsecond integers (`observed_at`).
   - Comparison now compares truncated microsecond values (`UnixMicro()`), preventing false mismatches while keeping exact payload bytes untouched.
8. **Removed Dead Fields:**
   - Removed unused `closed`, `closeOnce`, and `closeErr` from `edgeReplayPump`, leaving a cleaner, single-ownership model driven by the session context.
9. **Production Path Testing:**
   - Refactored ACK validation tests to invoke the production method (`pump.processEvent`) directly rather than copying `if` statements into test helpers.

---

## 3. Implemented Architecture & Protocol Invariants

### 3.1 Replay Pump Lifecycle & Goroutine Joining
- Single session-owned goroutine (`edgeReplayPump.run`) created inside `EdgeRuntime.Handle` when `replayRepo != nil`.
- Joined on session exit via `<-replayDone` alongside `<-healthDone`.
- All writes after handshake use `Session.SendReplay`; `conn.Write` is never called directly.

### 3.2 Hub Readiness Gating & Idle Bounding
- Replay begins and retransmits only after receiving a hub health frame with:
  - `Role == "hub"`
  - `Ready == true`
  - `DBWritable == true`
  - `Errors` does not contain `"ingest_unavailable"`
- When the outbox is empty or the hub is not ready, the pump executes a bounded idle sleep of ~250ms (`replayIdleWait`), while continuing to listen for incoming ACKs.

### 3.3 Handshake Welcome Cursor & Pruning Invariants
- Handshake validates that the hub's `welcome.CommittedSeq` cannot be below the edge's durable `CommittedSeq` (which has already been pruned). If `welcome.CommittedSeq < i.CommittedSeq`, handshake fails immediately.
- Neither `welcome` nor `health` frames authorize pruning of edge telemetry rows. Pruning occurs exclusively via `CommitReplayACK` upon receiving a valid `telemetry.ack`.

### 3.4 Read Starting Point & Lost ACK Recovery
- Reads start at the local durable cursor + 1 (`ReadReplayBatch(ctx, 0, maxEvents, maxBytes)`), ignoring any higher welcome cursor.
- If an ACK was lost during a previous session/outage, replaying from local cursor + 1 enables the hub to safely process duplicates and re-ACK.

### 3.5 Exact Byte Preservation & Manual Framing
- Events are validated against row metadata (`seq`, `kind`, truncated microsecond `observed_at`) using `decodeTelemetryEvent`.
- Frame construction in `buildReplayBatchFrame` uses manual byte buffer concatenation rather than `json.Marshal` on `json.RawMessage`, ensuring that all original stored event bytes—including whitespace and formatting—are preserved byte-for-byte.
- Limits enforced:
  - Envelope reservation: 4096 bytes headroom (`MaxBatchBytes - 4096 = 520,192` bytes max payload).
  - Complete frame: `<= MaxBatchBytes` (512 KiB).
  - Max batch events: `<= MaxBatchEvents` (256).
  - Max single event: `<= MaxEventBytes` (64 KiB).

### 3.6 Single In-Flight Batch & Retries
- Exactly one batch in-flight at any time, registered on the pump prior to `Session.SendReplay`.
- Retransmission sends the **exact same frame bytes** without re-reading or advancing cursors.
- Retries trigger upon:
  - Retransmission timeout (5s) with exponential backoff (`[100ms, 30s]`).
  - `telemetry.retry` frame with backoff clamped to `[100ms, 30s]`.
- No busy-loops or unbounded queues.

### 3.7 Strict ACK Validation & Storage Fence Commit
An ACK must satisfy all of the following:
1. `envelope.ConnectionGeneration == session.cfg.Generation`
2. `ack.StreamID == inflight.streamID`
3. `int64(ack.CommittedSeq) == inflight.lastSeq` (full batch coverage)
4. `ack.AcceptedCount + ack.DuplicateCount + len(ack.Rejected) == len(inflight.itemCount)`
5. Every `rej.Seq` is within `[firstSeq, lastSeq]` and is unique.
6. ACK does not acknowledge unsent rows.

Upon validation:
- Committed atomically via `repo.CommitReplayACK` with `domain.EdgeReplayFence{HubID, ProbeID, StreamID, ConnectionGeneration}` and `domain.ProbeReplayResult`.
- If local commit fails, the session closes immediately, preserving outbox rows.
- On success, `inflight` is cleared and `lastCompletedBatch` is recorded.

### 3.8 Control Fairness & Session Reader Concurrency
- `telemetry.ack` and `telemetry.retry` route to the pump via a bounded channel (`eventCh`, capacity 16).
- Health notifications update thread-safe hub readiness and notify `readyCh` without blocking the reader loop.
- If the event queue is full, the session closes with an error rather than deadlocking the reader loop.
- Pump run failures are never swallowed; `EdgeRuntime.Handle` returns `fmt.Errorf("edge replay pump failed: %w", pumpErr)`.

---

## 4. Authored Test Suite (`edge_replay_test.go`)

The authored test suite covers:
1. `TestEdgeReplayPump_ExactBytesPreservation`: Validates that custom whitespace, tabs, and newlines in stored event payloads are preserved intact in the generated batch envelope.
2. `TestEdgeReplayPump_BuildReplayBatchFrameValidation`: Table-driven tests validating stream ID match, positive generation, event count limits (1..256), sequence continuity, payload size limits, and envelope bounds.
3. `TestEdgeReplayPump_ReadinessGating`: Validates that the pump remains paused when hub health is not ready, DB is not writable, `Role != "hub"`, or `"ingest_unavailable"` is present.
4. `TestEdgeReplayPump_ACKValidation_Table`: Table-driven tests directly calling `pump.processEvent` verifying rejection of:
   - Stale session generation
   - Mismatched stream ID
   - Partial batch cursor (`committed_seq != last_seq`)
   - Outcome count mismatch (`accepted + duplicate + len(rejected) != batch count`)
   - Out-of-bounds rejection sequence
   - Duplicate rejection sequence in same ACK
5. `TestEdgeReplayPump_DuplicateACK_Handling`: Validates safe ignoring of proven duplicate ACKs (including shifted accepted/duplicate counts) and rejection of mismatched duplicate ACKs.
6. `TestEdgeReplayPump_RetryCursorValidation`: Directly tests retry cursor bounds (no advancement beyond local cursor, no sequence above sent last) and `RetryAfterMS` clamping.
7. `TestEdgeReplayPump_WelcomeAndHealthNeverPrune`: Asserts that hub welcome and health frames never invoke `CommitReplayACK`.
8. `TestEdgeReplayPump_MicrosecondTimestampComparison`: Demonstrates that nanosecond payload timestamps match microsecond store timestamps when truncated to microseconds.
9. `TestEdgeReplayPump_WelcomeCursorBelowCommittedSeq`: Verifies handshake rejection when hub welcome cursor precedes edge durable committed sequence.
10. `TestEdgeRuntime_RealTLS_ReplayBatchAndACK`: End-to-end TLS 1.3 WebSocket integration test using `httptest.Server`, `EdgeRuntime`, pinned certificates, and real SQLite database operations, verifying handshake, health exchange, batch replay, exact byte delivery, ACK commit, and outbox row deletion.

---

## 5. Test Execution Status

> [!IMPORTANT]
> **TESTS NOT RUN BY ANTIGRAVITY.**\
> In strict accordance with the prompt and work contract, Antigravity operated under FILE TOOLS ONLY mode: no commands (`run_command`), permission changes, commits, browser, or models were invoked. Codex owns command execution, compilation, test runs, hub integration, and acceptance.

---

## 6. File Inventory

### Files Written / Modified by Antigravity:
1. `internal/adapters/probe/edge_replay.go` (new) — Replay pump implementation, batch builder, frame validation, ACK validation, retry backoff.
2. `internal/adapters/probe/edge_replay_test.go` (new) — Unit tests directly calling production methods and full TLS integration test with real SQLite store.
3. `internal/adapters/probe/runtime_session.go` (modified) — Added `replayRepo` and `SetReplayRepository`, welcome cursor check, pump startup and lifecycle joining, routing of `health`, `telemetry.ack`, `telemetry.retry`.
4. `docs/multi-region/M3_EDGE_RUNTIME_HANDOFF.md` (new) — This handoff document with review corrections.

---

## 7. Known Limitations & Out-of-Scope Items

- **Hub-side Replay Ingest:** Codex owns hub transport, `ProbeReplayService`, authority checks (`ProbeReplayAuthorization`), DB migration 054 (`probe_telemetry_receipts`), and receipt deduplication.
- **Whole M3 Scope:** Gap recovery/snapshot eviction, watchdogs, remote commands, auxiliary alerting, and fleet UI are explicitly out of scope for this slice.
- **Composition Root Wiring:** Codex wires the real `edge.Store` to `EdgeRuntime.SetReplayRepository` in probe startup (`cmd/probe/runtime.go`).
