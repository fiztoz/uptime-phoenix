# M2 Runtime Session & Connector Integration Review

**Author**: Antigravity (Gemini 3.8 Flash High)

**Date**: 2026-09-20

**Contract**: `docs/multi-region/M2_RUNTIME_SESSION_REVIEW_CONTRACT.md`

**Scope**: Read-only audit of edge authenticated runtime session and connector integration.

**Ownership**: Codex owns all source code, tests, bootstrap, CLI, scripts, and documentation. Antigravity edits only this review document. No commits, pushes, or deployments.

---

## 1. Executive Summary

This audit evaluates the implementation of the authenticated edge runtime session and hub connector integration across:
- `internal/adapters/probe/runtime_session.go` and `runtime_session_test.go`
- `internal/adapters/probe/hub_transport.go`
- `internal/core/services/probe_connector_service.go` and `probe_connector_service_test.go`
- `cmd/probe/runtime.go`
- `internal/adapters/repository/probe_connection.go` and `probe_connector.go`

The inspected codebase demonstrates strict adherence to protocol contracts: handshake negotiation, generation fencing, atomic configuration activation, and lease renewal revocation operate as specified. No source changes are introduced in this review. Full integration and multi-process acceptance remain owned by Codex.

---

## 2. Core Mechanism Traces

### 2.1 Actual Shutdown
- **`cmd/probe/runtime.go` (`serveEdge`)**:
  - `serveEdge` sets up `runCtx, cancel := context.WithCancel(ctx)`.
  - Workers (`schedule.Run`, `delivery.Run`, `server.ServeTLS`) are coordinated via `sync.WaitGroup`.
  - On `<-ctx.Done()` or worker failure, `cancel()` is triggered, followed by `runtime.Close()`, `server.Shutdown(shutdownCtx)` (falling back to `server.Close()`), and `workers.Wait()`.
- **`internal/adapters/probe/runtime_session.go` (`EdgeRuntime.Close`)**:
  - `Close()` acquires `r.mu.Lock()`, sets `r.closed = true`, and copies the `r.connections` map.
  - For every tracked WebSocket connection, it invokes the associated `context.CancelFunc` and calls `conn.CloseNow()`.
  - It then calls `r.handlers.Wait()`, ensuring all in-flight `Handle` goroutines terminate before `Close()` returns.
  - In `Handle`, `sendHealth` runs in a separate goroutine bound to `establishedCtx`. When `session.Run` returns, `defer end()` cancels `establishedCtx`, and `<-healthDone` ensures the health goroutine exits cleanly before `Handle` finishes.
  - In-flight `ConfigTransfer` staging is safely discarded via `defer func() { if transfer != nil { transfer.Discard() } }()`.

### 2.2 Failed Lease Renewal
- **`internal/core/services/probe_connector_service.go` (`connectOnce`)**:
  - A background renewal goroutine runs with a 15-second ticker:
    ```go
    checkCtx, cancel := context.WithTimeout(sessionCtx, 5*time.Second)
    _, err := s.leases.RenewConnector(checkCtx, lease)
    if err == nil {
        next, readErr := s.configs.LatestMetadata(checkCtx, domain.ProbeConfigTarget{HubID: s.hubID, ProbeID: probeID})
        if readErr != nil && !errors.Is(readErr, ports.ErrNotFound) {
            err = readErr
        } else if next.Revision != metadata.Revision {
            err = ports.ErrConflict
        }
    }
    cancel()
    if err != nil {
        stop() // cancels sessionCtx
        return
    }
    ```
  - If `RenewConnector` fails (e.g. database error, lease expired, or lease stolen by another owner/generation), or if the hub config revision changes (`next.Revision != metadata.Revision`), `stop()` is invoked immediately.
  - Cancelling `sessionCtx` propagates into `transport.Run`, causing `session.Run` to exit, tearing down the WebSocket connection via `defer session.Close()`.
  - Upon exiting `connectOnce`, `defer s.release(lease)` calls `leases.ReleaseConnector(ctx, lease)` to clear the lease record in the hub database.

### 2.3 Lost Enrollment Receipt
- **`internal/core/services/probe_connector_service.go` (`Prepare` & `Enroll`)**:
  - `Prepare` generates the runtime token, seals it using `protector.SealCredential`, sets `State = "prepared"`, and commits the row to `probe_connections` prior to initiating any network request.
  - `Enroll` executes `s.transport.Enroll`. If the network drops or the enrollment receipt is lost/corrupted:
    - `Enroll` returns an error, but the credential record remains in `State = "prepared"`.
    - Subsequent calls to `Prepare` with matching metadata (`HubID`, `StreamID`, `Endpoint`, `Fingerprint`) detect the existing row and reuse it without generating a new credential.
    - Subsequent connection attempts in `connectOnce` dial the probe using the stored prepared credential.
    - When the probe connects and emits its first valid `health` frame, `HubTransport.Run` invokes the `established` callback, which calls `s.connections.ActivateConnection(...)`, safely transitioning state to `"active"`.

### 2.4 Stale Session Close
- **`internal/adapters/probe/runtime_session.go` (`Handle`)**:
  - Deferred cleanup in `Handle`:
    ```go
    defer func() {
        _ = session.Close()
        r.mu.Lock()
        if r.active == session {
            r.active = nil
        }
        r.mu.Unlock()
    }()
    ```
  - If session $S_1$ is replaced by a higher-generation session $S_2$, `r.active` is updated to $S_2$ before $S_1$ is closed.
  - When $S_1$'s `Handle` goroutine exits, its deferred cleanup checks `if r.active == session`. Since `r.active == S2`, $S_1$ does not set `r.active` to `nil`.
  - $S_1$'s socket is closed with `CloseNow()`, its session context is cancelled, and any subsequent write returns `session is closed`.

### 2.5 Higher-Generation Replacement vs Incumbent Survival
- **Incumbent Survival on Invalid Newcomer**:
  - If a newcomer connects with `generation <= current_generation`, `r.identity.AcceptConnectionGeneration(handshakeCtx, i.HubID, int64(welcome.ConnectionGeneration))` executes against `internal/adapters/repository/edge/store.go:215`.
  - `store.go:215` checks `if i.HubID != hubID || generation <= i.ConnectionGeneration { return ports.ErrConflict }`.
  - The database rejects the transaction with `ports.ErrConflict`.
  - `Handle` intercepts this, closes the newcomer's session with `ErrHandshakeGeneration`, and leaves `r.active` untouched. The incumbent session continues uninterrupted.
- **Higher-Generation Replacement**:
  - If a newcomer connects with `generation > current_generation`, `AcceptConnectionGeneration` persists the new fence in `edge_identity.connection_generation`.
  - Under `r.mu.Lock()`, `previous := r.active; r.active = session` swaps the active pointer.
  - `previous.Close()` terminates the incumbent session cleanly.

### 2.6 Configuration Transfer Rejection
- **`internal/adapters/probe/runtime_session.go` (`Handle`)**:
  - Config transfer stages through `config.begin`, `config.chunk`, and `config.commit`.
  - If `NewConfigTransfer`, `AddChunk`, `CommitDocument`, or `configs.Apply` fails (e.g. schema error, capability mismatch, hash mismatch, or storage activation failure):
    - `reject(transferID)` is executed:
      ```go
      response, err := encodeFrame("config.rejected", welcome.ConnectionGeneration, ConfigRejected{
          ConfigTransferIdentity: id,
          Errors: []ConfigError{{Path: "/", Code: "invalid_config", Message: "Configuration could not be applied"}},
      })
      ```
    - The staging buffer is discarded via `transfer.Discard()`.
    - A redacted, bounded error frame is returned to the hub.
    - `config.applied` is **never** emitted unless `r.configs.Apply` succeeds and commits to durable storage.

### 2.7 Authority & Readiness Boundaries
- **Authentication $\neq$ Ready $\neq$ Config Applied**:
  - An HTTP connection authenticated with `Authorization: Bearer <runtimeToken>` is merely authorized to negotiate.
  - In `cmd/probe/runtime.go:48`, probe readiness is strictly defined as:
    ```go
    Ready: writable && healthy && revision > 0
    ```
    If `revision == 0` (no configuration applied), `Ready` is `false`.
  - In `internal/adapters/probe/hub_transport.go:206`, the Hub explicitly advertises:
    ```go
    Health{Role: "hub", Ready: false, DBWritable: true, ..., Errors: []string{"ingest_unavailable"}}
    ```
    reflecting that M2 does not ingest telemetry.
- **Callback Authority Protection**:
  - In `ProbeConnectorService.connectOnce`, the `established` callback calls `s.leases.SetConnectorConnected(callbackCtx, lease, true)`.
  - `SetConnectorConnected` in `internal/adapters/repository/probe_connector.go:166` verifies `matchesProbeSession(row, lease) && row.LeaseUntil > now`.
  - If the lease has expired or was superseded by a higher generation, `SetConnectorConnected` returns `ports.ErrConflict`.
  - `ActivateConnection` is only reached if lease validation succeeds, preventing stale callbacks from activating or usurping connector authority.

---

## 3. Failure-Injection Lesson: Statement-Targeted Faults

### 3.1 The Flaw of Broad `BEFORE UPDATE` Triggers
In earlier M2 delivery tests, testing rollback of the final sequence counter update used a broad trigger:
```sql
CREATE TRIGGER fail_update BEFORE UPDATE ON edge_identity
BEGIN
    SELECT RAISE(ABORT, 'forced failure');
END;
```
However, in `internal/adapters/repository/edge/store.go`, transactions begin with:
```go
_, err := tx.ExecContext(ctx, "UPDATE edge_identity SET id = id WHERE id = 1")
```
This statement is executed immediately to acquire SQLite's exclusive writer lock. Consequently:
1. The broad `BEFORE UPDATE` trigger fired on `UPDATE edge_identity SET id = id`, before any outcome events were written or any queue updates occurred.
2. The test only proved that an immediate abort on statement 0 leaves the database unchanged; it did not test rollback of subsequent work performed during the transaction.

### 3.2 Targeted Fault Injection with Column and Value Conditions
Codex corrected this pattern by targeting the exact final counter advance statement:
```sql
CREATE TRIGGER fail_update BEFORE UPDATE OF last_created_seq ON edge_identity
WHEN NEW.last_created_seq != OLD.last_created_seq
BEGIN
    SELECT RAISE(ABORT, 'forced counter update failure');
END;
```
This trigger:
1. Ignores the initial writer lock (`id = id`, where `last_created_seq` is not updated).
2. Fires specifically when `last_created_seq` is modified with a new value at the conclusion of the transaction.
3. Successfully verifies that failure at the final counter advance rolls back all preceding outcome inserts and queue modifications.

**Rule for all future failure injection**: When a transaction performs multiple operations or touches the same table repeatedly, triggers must specify the target column (`UPDATE OF column_name`) and condition (`WHEN NEW.col != OLD.col`) to guarantee the fault is injected at the intended write step.

---

## 4. Observed Evidence & Test Execution

Tests were executed using the required toolchain and cache environment:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 GOCACHE=/private/tmp/phoenix-go-cache-template-race
```

### 4.1 Probe Runtime Tests (`internal/adapters/probe`)
- **Command**:
  ```bash
  rtk proxy env GOTOOLCHAIN=go1.26.6 GOCACHE=/private/tmp/phoenix-go-cache-template-race go test -v -race -count=1 ./internal/adapters/probe -run "^TestEdgeRuntime"
  ```
- **Execution Notes**:
  - In standard sandbox mode, loopback TCP connection (`dial tcp 127.0.0.1:<port>`) is blocked by the OS sandbox policy (`connect: operation not permitted`).
  - Executed with bypass sandbox to permit local loopback socket communication.
- **Results**:
  - `TestEdgeRuntimeFencingConfigReceiptAndShutdown`: PASS (0.12s)
    - `TestEdgeRuntimeFencingConfigReceiptAndShutdown/duplicate`: PASS (0.01s)
    - `TestEdgeRuntimeFencingConfigReceiptAndShutdown/wrong_stream`: PASS (0.01s)
    - `TestEdgeRuntimeFencingConfigReceiptAndShutdown/cursor_ahead`: PASS (0.01s)
  - Exit code: `0`
  - Output log: `/private/tmp/antigravity-probe-runtime-test.log`

### 4.2 Probe Connector Service Tests (`internal/core/services`)
- **Command**:
  ```bash
  rtk proxy env GOTOOLCHAIN=go1.26.6 GOCACHE=/private/tmp/phoenix-go-cache-template-race go test -v -race -count=1 ./internal/core/services -run "^TestProbeConnector"
  ```
- **Results**:
  - `TestProbeConnectorPreparedCredentialsSurviveLostEnrollmentReceipt`: PASS (0.00s)
  - `TestProbeConnectorRejectsMissingCursorBeforeNetwork`: PASS (0.00s)
  - `TestProbeConnectorCannotActivateAfterFencedCallback`: PASS (0.00s)
  - `TestProbeConnectorLeaseRenewalFailureCancelsSocket`: PASS (15.00s) — verified socket cancellation upon lease renewal failure.
  - Exit code: `0`
  - Output log: `/private/tmp/antigravity-probe-connector-test.log`

### 4.3 Database Suites
- MariaDB suites were not executed (`TEST_MARIADB_DSN` omitted) per contract: Codex owns the shared disposable MariaDB database.

---

## 5. Actionable Findings vs Untested Concerns

### 5.1 Untested Concerns

1. **Transient Health Evaluation Error Drops Active Session**:
   - **Location**: `internal/adapters/probe/runtime_session.go:245-261` (`sendHealth`)
   - **Mechanism**:
     ```go
     h, err := r.health(opCtx)
     if err == nil && h.Role == "probe" {
         // send health frame
     } else {
         err = errors.New("local health unavailable")
     }
     cancel()
     if err != nil {
         _ = s.Close()
         return
     }
     ```
   - **Observation**: If `r.health(opCtx)` fails once due to a transient condition (e.g. SQLite busy or temporary diagnostic read timeout in `store.ReadDiagnostics`), `sendHealth` immediately terminates the session via `s.Close()`.
   - **Impact**: While failing closed preserves safety, a single transient error forces a full WebSocket reconnection and re-handshake rather than retrying on the next heartbeat tick.

2. **Adapter Mutex Held Across SQLite Transaction**:
   - **Location**: `internal/adapters/probe/runtime_session.go:120`
   - **Mechanism**:
     ```go
     r.mu.Lock()
     ...
     err = r.identity.AcceptConnectionGeneration(handshakeCtx, i.HubID, int64(welcome.ConnectionGeneration))
     if err != nil {
         r.mu.Unlock()
         _ = session.Close()
         return ErrHandshakeGeneration
     }
     r.active = session
     r.mu.Unlock()
     ```
   - **Observation**: `AcceptConnectionGeneration` performs a SQLite transaction (`s.write`) while holding `r.mu.Lock()`.
   - **Impact**: Any concurrent call to `EdgeRuntime.Close()` or another incoming `Handle` connection will block waiting for `r.mu.Lock()` until the database transaction completes. Because `handshakeCtx` has a 10s deadline and SQLite writes are local and fast, this is not a deadlock, but holding adapter locks across I/O is notable.

3. **In-Flight `sendItem` on `SendControl` Timeout**:
   - **Location**: `internal/adapters/probe/session.go:157-172`
   - **Mechanism**:
     When `s.SendControl(ctx, frame)` enqueues into `s.controlQueue`, if `ctx` is cancelled before `<-item.done` receives the writer result, `SendControl` returns `ctx.Err()`. However, `item` may still be processed by `writerLoop`. The writer writes to `item.done` (buffered chan of size 1), so no goroutine leaks, but the caller cannot know whether the frame reached the wire.

---

## 6. Conclusion & Boundaries

The edge authenticated runtime session and connector service correctly enforce generation fencing, lease-bound authority, and atomic configuration application. No regressions or architectural violations were identified in the audited code paths.

Codex will proceed with multi-process integration tests and the final integration gate.
