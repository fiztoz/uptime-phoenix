# M3 Completion Requirements Audit

> **Advisory Report for Integrator/Codex** | Baseline: `eb5c59e` (branch `codex/multi-region-probe-plan`) | Status: **IN PROGRESS**
> This baseline audit predates the retention implementation; follow the work contract and newest acceptance report for current status.
> Verification citations reflect historical Codex test execution; no tests were executed during this audit.

## 1. Requirements & Baseline Evidence Matrix

| Requirement / Acceptance Area | Status | Implemented Foundation & Historical Evidence | Missing Seams & Concrete Constraints |
|---|---|---|---|
| **1. Config Snapshots & Publication** | Complete | Snapshots/receipts in `047`/`049`; [`RemoteProbeConfigSyncStore`](../../internal/adapters/repository/probe_config_sync.go); verified by Codex in `TestRemoteConfigSyncContract`. | None for M3 baseline. |
| **2. Chunked Staging & Activation** | Complete | Assembler in [`ChunkAssembler`](../../internal/adapters/probe/config_assembler.go); runtime apply in [`EdgeConfigService`](../../internal/core/services/edge_config_service.go); verified in `M3_CONFIG_SYNC_ACCEPTANCE.md`. | None for M3 baseline. |
| **3. Ordered Replay & Gaps** | Partial | Batch ingest in [`ProbeReplayStore`](../../internal/adapters/repository/probe_replay.go); receipts in `054`; outbox pump in [`edgeReplayPump`](../../internal/adapters/probe/edge_replay.go); verified in `TestProbeReplayAcceptance`. | Retention eviction (target 512 MiB / 7 days, 80% warning, >=10% reserve); `edge_gaps` table; runtime handling of existing `telemetry.gap` and `telemetry.retry` framing. |
| **4. Current-State Snapshots** | Foundation | Wire DTOs in [`state.go`](../../internal/adapters/probe/state.go); staging in [`StateTransfer`](../../internal/adapters/probe/state_assembler.go). | Edge snapshot generation/sender; hub state projection ingest transaction and missing-state UNKNOWN reconciliation. |
| **5. Monotonic Guards & Dirty Buckets** | Partial | Monotonic guards in `updateReplayState` (`internal/adapters/repository/probe_replay.go`); event authority in `AccessService`; `probe_dirty_buckets` written by [`markDirtyTx`](../../internal/adapters/repository/probe_replay.go). | `ProcessDirty` is not ready-to-run: rollup consumer (`1m`/`1h`/`1d`), multi-resolution evidence trace, and generation race safety remain unwired. |
| **6. Connection Watchdogs** | Foundation | Health framing in [`health.go`](../../internal/adapters/probe/health.go); transition DTOs in [`incidents.go`](../../internal/adapters/probe/incidents.go). | Edge watchdog (90s loss / 30s recovery); hub connection tracking (45s suspect / 90s disconnect); `watchdog.transition` unhandled in replay. |
| **7. Runnable Command Controls** | Foundation | Request/result DTOs in [`commands.go`](../../internal/adapters/probe/commands.go); `probe_commands` table (`036`). | Runnable admin control (via [`cmd/phoenix-probe-admin`](../../cmd/phoenix-probe-admin)); edge `edge_applied_commands`; command transport; offline `alert.ack`. Full fleet UI/API deferred to M5. |
| **8. Fenced Stream Reset & Rotation** | Foundation | Rotation DTOs in [`commands.go`](../../internal/adapters/probe/commands.go); manual enrollment in [`enrollment.go`](../../internal/adapters/probe/enrollment.go). | Operator-authorized fenced recovery: distinguish hub restore gaps from edge rollback identity recovery; credential/cert rotation orchestration. |
| **9. Queue Diagnostics & Flushing** | Foundation | Diagnostics in [`Store.ReadDiagnostics`](../../internal/adapters/repository/edge/diagnostics.go). | Queue pressure warning (80% threshold); bounded outbox drain loop during edge SIGINT/SIGTERM in [`cmd/probe`](../../cmd/probe). |
| **10. 15-Min Partition Acceptance** | Partial | Historical outbox replay verified for short windows in `M3_REPLAY_ACCEPTANCE.md`. | End-to-end 15-min partition requires state snapshot jumping ahead of backlog, offline `alert.ack`, and dual watchdogs. |

## 2. Verified Missing Seams

- **Retention & Declared Gaps**: Edge outbox rejects at capacity ([`internal/adapters/repository/edge/record.go`](../../internal/adapters/repository/edge/record.go) returns `ErrQueueFull`) rather than evicting aged/excess telemetry. Missing `edge_gaps` storage, `telemetry.gap` emission/ingestion, and hub `telemetry.retry` backoff response. Target limits: 512 MiB / 7 days, warning at 80%, reserve at least 10%.
- **Current-State Projection**: [`EdgeRuntime.Handle`](../../internal/adapters/probe/runtime_session.go) and [`HubTransport.Run`](../../internal/adapters/probe/hub_transport.go) return unsupported for state frames. Hub cannot project fresh health ahead of replaying backlogs.
- **Dirty Bucket Rollup & Concurrency**: While `probe_dirty_buckets` records dirty ranges, `AggregateService` lacks a rollup worker consuming those marks for `1m`, `1h`, and `1d`. [`MonitorHealthService.ProcessDirty`](../../internal/core/services/monitor_health.go) cannot be considered ready-to-run until resolution tracing, input evidence sources, and generation race safety against live inserts are audited and wired.
- **Connection Watchdogs**: Neither edge watchdog nor hub connection monitor is running. `sendHealth` ticks every 15s, but 90s failure / 30s recovery state transitions and startup arming are unimplemented; replay decoder rejects `watchdog.transition`.
- **Command Controls & Offline ACK**: M3 requires runnable command controls (using [`cmd/phoenix-probe-admin`](../../cmd/phoenix-probe-admin) as composition root; full fleet UI/API deferred to M5). Missing hub command queue/sender, edge `edge_applied_commands` storage, and offline `alert.ack` execution.
- **Fenced Stream Reset & Rotation**: Stream reset must be explicit, operator-authorized, fenced recovery. Distinguish hub backup restore (where hub lost cursors and requires gap declaration / stream reconciliation) from edge rollback (where edge restored old DB and requires a new stream epoch with hub authorization). Automatic stream reset or queue deletion on `stream_reset_required` is forbidden. Credential/cert rotation orchestration is missing.
- **Queue Pressure & Shutdown**: No evaluation of the 80% outbox warning threshold. Probe runtime shuts down immediately on signal without bounded outbox flushing.

## 3. Dependency Order

1. **3A: Retention Eviction, Gaps & Hub Retry**: Edge migration for `edge_gaps`, 512 MiB / 7-day retention pruning with >=10% reserve, `telemetry.gap` frame, hub gap ingestion, and hub `telemetry.retry` backoff.
2. **3B: Current-State Snapshots**: Edge snapshot builder/sender (`state.begin/chunk/commit`), hub transactional state projection, and missing-assignment UNKNOWN derivation.
3. **3C: Dirty Bucket Rollup Worker**: Trace evidence and race safety, wire background worker in [`internal/bootstrap/run.go`](../../internal/bootstrap/run.go), and implement `AggregateService` rollups across `1m`, `1h`, and `1d`.
4. **3D: Dual Connection Watchdogs**: Edge watchdog with direct alert emission, hub connection monitor (45s suspect / 90s disconnect), and replay support for `watchdog.transition`.
5. **3E: Runnable Command Controls & Offline ACK**: Admin CLI commands in [`cmd/phoenix-probe-admin`](../../cmd/phoenix-probe-admin), hub command dispatch, edge `edge_applied_commands` table, and `alert.ack` execution.
6. **3F: Fenced Stream Reset & Rotation**: Operator-authorized reset tooling (distinguishing hub restore vs. edge rollback) and 10-minute overlap credential/cert rotation.
7. **3G: Queue Pressure & 15-Min Partition Verification**: 80% queue pressure warning, bounded shutdown outbox flush, and full 15-min partition acceptance run.
