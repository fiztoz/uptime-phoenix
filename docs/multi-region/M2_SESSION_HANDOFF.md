# M2 Bounded Established-Session Transport Handoff

## Summary

This handoff documents the implementation and verification of the bounded established-session transport slice for M2, complying with `docs/multi-region/M2_SESSION_WORK_CONTRACT.md` and resolving all findings from `docs/multi-region/M2_SESSION_REVIEW.md`.

## Owned Files

- `internal/adapters/probe/session.go`
- `internal/adapters/probe/session_test.go`
- `docs/multi-region/M2_SESSION_HANDOFF.md`

(Prerequisite error-redaction fix in `internal/adapters/probe/runtime_identity.go`, `runtime_identity_test.go`, and `docs/multi-region/M2_IDENTITY_HANDOFF.md` was completed and frozen in the preceding step.)

No Codex-owned files (`edge_server.go`, persistence, core domain/ports/services, or migrations) were touched.

## Frozen API Implementation

The frozen API from `docs/multi-region/M2_SESSION_WORK_CONTRACT.md` is strictly implemented:

```go
type SessionConfig struct {
    Generation Decimal
    PeerRole   string
}

type Session struct { ... }

func NewSession(conn *websocket.Conn, cfg SessionConfig) (*Session, error)
func (s *Session) Run(ctx context.Context, handle func(context.Context, Envelope) error) error
func (s *Session) SendControl(ctx context.Context, frame []byte) error
func (s *Session) SendReplay(ctx context.Context, frame []byte) error
func (s *Session) Close() error
func ReconnectDelay(consecutiveFailures int, healthyDuration time.Duration, randomFloat float64) time.Duration
```

## Resolution of Review Findings

1. **Context Initialization and Race Elimination (`NewSession`, `Run`, `Close`)**:
   - `sessCtx` and `cancelSess` are created during `NewSession`, ensuring a valid, non-nil session cancellation context before `Run` or `Close` can be invoked.
   - `Close()` acquires `s.mu`, marks `s.isClosed = true`, closes `s.closed`, invokes `s.cancelSess()`, and immediately closes the underlying websocket with `CloseNow()`.
   - `Run()` uses `context.AfterFunc(s.sessCtx, cancel)` to link external closure to the active run loop without data races.
   - Calling `Close()` before `Run()` causes `Run()` to fail immediately with `"session is closed"`.
   - Verified by `TestSession_ConcurrentRunAndClose` and `TestSession_CloseBeforeRun` under `-race`.

2. **In-Flight Write Cancellation (`writerLoop`, `writeItem`)**:
   - In-flight writes derive a combined context from `runCtx` and `item.ctx` using `context.AfterFunc(item.ctx, writeCancel)` with cleanup on return, avoiding extra goroutines.
   - If a write is canceled or fails, `s.Close()` is immediately triggered, terminating the socket via `CloseNow()` and unblocking all queued siblings with `"session closed"`.
   - Verified by `TestSession_InFlightCancellationTerminatesSocketAndSiblings`.

3. **Strict Error Redaction**:
   - Peer close reasons (`websocket.CloseError`) are never reflected back in error strings (redacted to `"websocket read failed"` or status code descriptions).
   - Malformed frames and invalid JSON input are strictly redacted to generic errors (`"invalid JSON frame"`, `"invalid control frame"`, `"invalid replay frame"`) without echoing caller payload or sensitive strings.
   - Health frame decoding strictly checks sender role against `PeerRole` (`"probe"` vs `"hub"`) and rejects mismatches.
   - Verified by `TestSession_IncomingFrameRejections` (including `peer_close_reason_redacted`) and `TestSession_ValidationFailures/malformed_json_redacted`.

4. **Bounded Queues, Priority, and Fairness**:
   - Separate 16-element bounded channels: `controlQueue chan *sendItem` and `replayQueue chan *sendItem`.
   - `nextItem` checks `ctx.Done()` and `s.closed` first before selecting from queues, preventing starvation and honoring caller cancellation even under high load.
   - Fairness rule enforced: after at most 8 consecutive control frames, 1 waiting replay frame is dequeued if available. If either queue is empty, the other progresses immediately.
   - Verified by `TestSession_FairnessAndPriority`.

5. **Byte Slice Cloning & Allocation Isolation**:
   - `SendControl` and `SendReplay` clone the caller's byte slice (`bytes.Clone(frame)`) before decoding or queuing, ensuring mutations by the caller cannot alter queued or in-flight data.
   - Verified by `TestSession_ByteSliceOwnership`.

6. **In-Memory Pipe Transport in Tests**:
   - `session_test.go` implements `pipeListener` using `net.Pipe()` and standard `http.Server.Serve` with `websocket.Accept` and `websocket.Dial`.
   - Executes 100% in-memory with zero OS network sockets (`socket()`, `bind()`, `connect()`), guaranteeing full test pass within the sandboxed environment.

## Observed Verification Evidence

### 1. Focused Session and ReconnectDelay Unit Tests (Race Detector Enabled)

Command:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -v -race -count=1 ./internal/adapters/probe -run "^TestSession|^TestReconnectDelay"
```

Output:
```
=== RUN   TestSessionFramesRequiredFieldsAndWireRoundTrip
=== RUN   TestSessionFramesRequiredFieldsAndWireRoundTrip/hello-active.json
=== RUN   TestSessionFramesRequiredFieldsAndWireRoundTrip/welcome-active.json
=== RUN   TestSessionFramesRequiredFieldsAndWireRoundTrip/health-hub-ingest.json
--- PASS: TestSessionFramesRequiredFieldsAndWireRoundTrip (0.03s)
    --- PASS: TestSessionFramesRequiredFieldsAndWireRoundTrip/hello-active.json (0.01s)
    --- PASS: TestSessionFramesRequiredFieldsAndWireRoundTrip/welcome-active.json (0.01s)
    --- PASS: TestSessionFramesRequiredFieldsAndWireRoundTrip/health-hub-ingest.json (0.01s)
=== RUN   TestSessionFramesShareEnvelopeGuards
=== RUN   TestSessionFramesShareEnvelopeGuards/hello-active.json
=== RUN   TestSessionFramesShareEnvelopeGuards/welcome-active.json
=== RUN   TestSessionFramesShareEnvelopeGuards/health-hub-ingest.json
--- PASS: TestSessionFramesShareEnvelopeGuards (0.01s)
    --- PASS: TestSessionFramesShareEnvelopeGuards/hello-active.json (0.01s)
    --- PASS: TestSessionFramesShareEnvelopeGuards/welcome-active.json (0.00s)
    --- PASS: TestSessionFramesShareEnvelopeGuards/health-hub-ingest.json (0.00s)
=== RUN   TestSession_NewSessionValidation
--- PASS: TestSession_NewSessionValidation (0.00s)
=== RUN   TestSession_SingleRun
--- PASS: TestSession_SingleRun (0.02s)
=== RUN   TestSession_ConcurrentRunAndClose
--- PASS: TestSession_ConcurrentRunAndClose (0.00s)
=== RUN   TestSession_CloseBeforeRun
--- PASS: TestSession_CloseBeforeRun (0.00s)
=== RUN   TestSession_SendControlAndReceive
--- PASS: TestSession_SendControlAndReceive (0.00s)
=== RUN   TestSession_SendReplayAndReceive
--- PASS: TestSession_SendReplayAndReceive (0.00s)
=== RUN   TestSession_ValidationFailures
=== RUN   TestSession_ValidationFailures/frame_exceeds_max_bytes
=== RUN   TestSession_ValidationFailures/malformed_json_redacted
=== RUN   TestSession_ValidationFailures/generation_mismatch
=== RUN   TestSession_ValidationFailures/handshake_frame_forbidden
=== RUN   TestSession_ValidationFailures/control_rejects_replay_types
=== RUN   TestSession_ValidationFailures/replay_rejects_non-replay_types
--- PASS: TestSession_ValidationFailures (0.00s)
    --- PASS: TestSession_ValidationFailures/frame_exceeds_max_bytes (0.00s)
    --- PASS: TestSession_ValidationFailures/malformed_json_redacted (0.00s)
    --- PASS: TestSession_ValidationFailures/generation_mismatch (0.00s)
    --- PASS: TestSession_ValidationFailures/handshake_frame_forbidden (0.00s)
    --- PASS: TestSession_ValidationFailures/control_rejects_replay_types (0.00s)
    --- PASS: TestSession_ValidationFailures/replay_rejects_non-replay_types (0.00s)
=== RUN   TestSession_IncomingFrameRejections
=== RUN   TestSession_IncomingFrameRejections/generation_mismatch_closes_session
=== RUN   TestSession_IncomingFrameRejections/health_role_mismatch_closes_session
=== RUN   TestSession_IncomingFrameRejections/binary_frame_rejected
=== RUN   TestSession_IncomingFrameRejections/peer_close_reason_redacted
--- PASS: TestSession_IncomingFrameRejections (0.00s)
    --- PASS: TestSession_IncomingFrameRejections/generation_mismatch_closes_session (0.00s)
    --- PASS: TestSession_IncomingFrameRejections/health_role_mismatch_closes_session (0.00s)
    --- PASS: TestSession_IncomingFrameRejections/binary_frame_rejected (0.00s)
    --- PASS: TestSession_IncomingFrameRejections/peer_close_reason_redacted (0.00s)
=== RUN   TestSession_ByteSliceOwnership
--- PASS: TestSession_ByteSliceOwnership (0.00s)
=== RUN   TestSession_CallerContextCancellationWhileQueued
--- PASS: TestSession_CallerContextCancellationWhileQueued (0.00s)
=== RUN   TestSession_CloseUnblocksPendingSends
--- PASS: TestSession_CloseUnblocksPendingSends (0.02s)
=== RUN   TestSession_FairnessAndPriority
--- PASS: TestSession_FairnessAndPriority (0.00s)
=== RUN   TestSession_InFlightCancellationTerminatesSocketAndSiblings
--- PASS: TestSession_InFlightCancellationTerminatesSocketAndSiblings (0.02s)
=== RUN   TestReconnectDelay
=== RUN   TestReconnectDelay/failure_1_zero_jitter
=== RUN   TestReconnectDelay/failure_1_max_jitter
=== RUN   TestReconnectDelay/failure_2_mid_jitter
=== RUN   TestReconnectDelay/failure_3_mid_jitter
=== RUN   TestReconnectDelay/failure_4_mid_jitter
=== RUN   TestReconnectDelay/failure_5_mid_jitter
=== RUN   TestReconnectDelay/failure_6_mid_jitter
=== RUN   TestReconnectDelay/large_failure_clamped_to_30s
=== RUN   TestReconnectDelay/negative_failure_treated_as_1
=== RUN   TestReconnectDelay/healthy_>=_30s_resets_to_failure_1
=== RUN   TestReconnectDelay/healthy_<_30s_does_not_reset
=== RUN   TestReconnectDelay/random_<_0_clamped_to_0
=== RUN   TestReconnectDelay/random_>_1_clamped_to_1
=== RUN   TestReconnectDelay/random_NaN_clamped_to_1
--- PASS: TestReconnectDelay (0.00s)
    --- PASS: TestReconnectDelay/failure_1_zero_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_1_max_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_2_mid_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_3_mid_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_4_mid_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_5_mid_jitter (0.00s)
    --- PASS: TestReconnectDelay/failure_6_mid_jitter (0.00s)
    --- PASS: TestReconnectDelay/large_failure_clamped_to_30s (0.00s)
    --- PASS: TestReconnectDelay/negative_failure_treated_as_1 (0.00s)
    --- PASS: TestReconnectDelay/healthy_>=_30s_resets_to_failure_1 (0.00s)
    --- PASS: TestReconnectDelay/healthy_<_30s_does_not_reset (0.00s)
    --- PASS: TestReconnectDelay/random_<_0_clamped_to_0 (0.00s)
    --- PASS: TestReconnectDelay/random_>_1_clamped_to_1 (0.00s)
    --- PASS: TestReconnectDelay/random_NaN_clamped_to_1 (0.00s)
PASS
ok  	github.com/fiztoz/uptime-phoenix/internal/adapters/probe	1.865s
```
Exit Code: 0.

### 2. Static Analysis & Linter Verification

- `gofmt -l internal/adapters/probe/session*.go`: output is empty.
- `go vet ./internal/adapters/probe/...`: exited 0 with no warnings.
- `GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run ./internal/adapters/probe/...`: zero issues in owned code (`session.go`, `session_test.go`, `runtime_identity.go`, `pinned_client.go`). Only 1 preexisting misspell in Codex-owned `config_assembler.go`.

## Boundary Commitments

- No commits, pushes, or deployments.
- No modifications to unowned files.
- This slice alone does not complete M2.
