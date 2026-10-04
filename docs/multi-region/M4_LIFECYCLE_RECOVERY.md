# M4 lifecycle and recovery contracts

This record defines the clear-history watermark, soft-delete behavior,
assignment removal and tombstones, stream retirement, and the restored-hub and
restored-edge recovery procedures. It is the "define clear-history watermarks,
soft-delete behavior, assignment removal/tombstones, stream retirement, and
restored-hub/edge recovery procedures" bullet of [M4](IMPLEMENTATION_PLAN.md).
Deployment documentation remains open.

| Topic | Status |
|---|---|
| Clear-history watermarks | **Implemented and tested** (hub side). Source-side `history.clear` command dispatch/application and regional-only clear scopes remain defined but unimplemented |
| Soft-delete behavior | **Defined**. The fleet `DELETE /api/probes/:probe_id` surface remains a proposed M5 endpoint |
| Assignment removal / tombstones | **Implemented and tested** (M3/M4) |
| Stream retirement | **Implemented and tested** (M3 source reset) |
| Restored-hub / restored-edge recovery | **Procedures defined** below; the tooling they call exists (probe-admin, stream reset, clear history) |

## 1. Clear-history watermarks

The authorized clear-history action is `DELETE /api/monitors/:id/heartbeats`
(admin-only). It is one deliberate, atomic deletion of the monitor's **history
evidence**: raw `heartbeats`, the `heartbeat_1m/1h/1d` rollups, regional
`probe_observations`, the materialized `monitor_health_history` windows and the
pending `probe_dirty_buckets` recompute work.

Not history, and never touched by a clear: current state
(`monitor_health_state`, `monitor_probe_state`), incidents, delivery outcomes,
assignment rows and tombstones, and `probe_telemetry_receipts`. The action
refuses an unknown monitor instead of deleting nothing and reporting success.

**The watermark.** One row per `(monitor_id, probe_id, assignment_generation)`
in `history_clear_watermarks` (migration `069`) records the explicit bound the
operator cleared through:

- `through_seq` — the highest sequence ingested for that monitor and probe at
  clear time;
- `through_observed_at` — the clear time (UTC, microsecond precision);
- `clear_id` — the most recent clear's identity;
- `dropped_count` — the acknowledged intentional drops under this fence.

An observation of that identity is **cleared** when `seq <= through_seq` **or**
`observed_at <= through_observed_at`. The sequence bound is deterministic and
covers a hub restored behind the clear that receives replayed old sequence
space; the observation bound covers evidence the source created before the
clear but that had not been ingested yet — the ordinary replayed/backlogged
queue. Both bounds are compared at the persisted precision (UTC, microseconds).

A backward **source** clock can make fresh evidence look pre-clear; the drop is
the conservative choice — resurrecting removed evidence is the worse failure —
and every drop is counted on the fence. A backward **hub** clock can never
shrink a fence: repeated clears raise both bounds (`max`), never lower them.

**Enforcement.** Both hub ingest paths refuse fenced evidence: the ordered
replay path (`IngestReplayBatch`) and the contiguous-prefix path (`Ingest`).
The cursor still advances — the event was consumed — but no observation is
inserted, no state moves and no history bucket is dirtied. In the replay path
the refusal is recorded as a `probe_telemetry_receipts` row with rejection code
`history_cleared`, so a later replay of the same prefix is discarded
**idempotently** from its receipt and the source is told its evidence was
dropped (an acknowledged intentional drop, not a silent loss).

**Generation scope.** A recreated assignment (remove then re-add) advances the
generation and starts a fresh evidence identity. The previous generation's
fence never covers it; its own fence is retained untouched.

Defined but **not implemented** in this slice: dispatching and applying the
source-side `history.clear` command (the wire codec exists and is specified in
[PROTOCOL](PROTOCOL.md); the source does not yet evict its retained copy), and
regional-only clear scopes — the endpoint clears overall. The hub-side fence
already provides the T36 guarantee ("clear watermark prevents resurrecting
removed evidence") for every replay path.

## 2. Soft-delete behavior (defined)

- **Probes.** `DELETE /api/probes/:probe_id` (proposed fleet API, M5) returns
  409 while the probe is actively assigned; otherwise it soft-deletes and
  revokes the identity while retaining historical attribution. The reserved
  `local` registration can never be deleted. Registration rows are otherwise
  upsert-only: config-as-code prune never deletes them, and backup/restore
  never mints a duplicate live identity.
- **Monitors.** Deleting a monitor removes its history with it (assignment
  sets, tombstones and clear-history watermarks cascade) and its evidence can
  never recreate it: ingest of a removed monitor is a permanent rejection
  recorded as a receipt.
- **Old-generation results** may enter historical storage within the valid
  assignment interval and the retention horizon; they cannot update live state
  or recreate a removed monitor.

## 3. Assignment removal and tombstones (implemented)

One desired set per monitor, replaced atomically under an optimistic revision.
Removed members are **tombstoned**: their generation is retained and can never
be reused; re-adding a member increments its generation. A no-op replacement
preserves the revision. Membership and policy intervals are committed to
`monitor_probe_assignment_history` with the desired set; an empty history means
unknown membership, never today's set applied backwards. Reads never create
assignments. A set without an active `local` member is remote-only: the hub
scheduler must not claim it (`LocalHubExecutionSQL`).
Declarative commits (`Restore`, used by backup restore and config apply) share
all of these rules and differ from operator replacement only in that members
must be *registered* rather than *enabled*.

Verified by `TestProbeRegistryContract/{sqlite,mariadb}`
(`RegistryAndIdentity`, `ReplacementAndTombstones`, `ConcurrentReplacement`,
`AtomicRollbackAndOverflow`), `TestMonitorCreateInitializesLocalAssignment`,
`TestProbeAssignmentRestore_*`, and the assignment-history tests.

## 4. Stream retirement (implemented)

Retirement is explicit and never inferred. A stream's cursor state is never
deleted while queued retries could arrive. `prepare-reset`/`activate-reset`
(probe-admin) retire the old epoch and open a new stream epoch under hub
authorization, so an edge restored from an old snapshot can never reuse old
sequence numbers (T35). Retired streams refuse ingest. Rejection and gap
receipts live outside the partitioned heartbeats and are bounded by the
retention/stream-retirement policy. Declared gaps (`telemetry.gap`, reasons
including `retention_bytes`, `retention_age`, `disk_pressure`, `restore_loss`
and `history_cleared`) mark affected coverage unknown instead of inventing
data; ordinary retention gaps cannot erase committed rows.

See [M3 source reset acceptance](M3_SOURCE_RESET_ACCEPTANCE.md) and
[hub reset acceptance](M3_HUB_RESET_ACCEPTANCE.md).

## 5. Restored-hub and restored-edge recovery procedures

### 5.1 Restoring a hub from backup

1. Quiesce connector workers (`PROBES_ENABLED=false` or stop the pods) before
   restoring. Restored runtime leases and connector fencing authority are only
   trustworthy after the restored database is the only writer again; renew or
   revoke connector fencing authority before the restored hub issues any
   command.
2. Reconcile `probe_streams` cursors against each probe's retained queue on
   reconnect: the hub advertises its committed cursor and the probe resends
   from the following sequence or declares retained-history loss. A hub
   restored behind a probe's retention floor receives explicit gaps
   (`restore_loss`) and current state; it cannot reconstruct data no longer
   stored anywhere.
3. **Re-run clear-history for any monitor whose history was cleared after the
   backup's timestamp.** A backup taken before a clear contains neither the
   cleared evidence nor its fence, so replayed pre-backup queues can resurrect
   rows the operator had removed. Re-running the clear removes them again and
   installs the fence for everything that follows. This step is what makes the
   T36 guarantee hold across a hub restore.
4. Reconcile inactive enrollment records: a restored hub must not accept an old
   enrollment token, and a probe whose credential was lost reenrolls through
   `phoenix-probe-admin register` + `enroll` (register adopts and re-enables a
   restored disabled identity).
5. Resume workers only after cursors and fences are reconciled.

### 5.2 Restoring an edge (probe) from snapshot

1. Stop the source first (`probe.stop` / service stop). Its SQLite snapshot and
   TLS/secret key material must be restored together — a snapshot without its
   key material is an identity loss and requires reenrollment.
2. An old snapshot carries old sequence numbers. Restoring it **requires an
   explicit new stream epoch**: `prepare-reset --probe-id … --previous-stream-id
   … --stream-id …`, then `probe reset-stream --plan-file` on the stopped
   source, then `activate-reset` with the receipt. Old sequence space is
   retired; nothing may reuse it.
3. Restart the source. It replays retained telemetry under the new epoch; the
   hub accepts it as new sequence space, and any evidence covered by a
   clear-history fence is still dropped and acknowledged (the fence is scoped
   by monitor/probe/generation, not by stream).
4. If the source's local identity or credential is missing, reenroll before
   reconnecting.

### 5.3 Combined hub + edge recovery order

Restore the hub first (steps 5.1.1–5.1.4), then each edge (5.2), then resume
workers. The hub's fences and cursors are authoritative; an edge may never talk
a restored hub into reusing retired sequence space or re-opening a cleared
window.

## Verification

Executed in this checkout:

- `go build ./...`, `go vet ./internal/...`, `gofmt -l internal/` — clean.
- `go test -count=1 ./internal/... ./cmd/...` — all packages ok.
- Targeted `-race` run with named passes on **both engines** (disposable
  MariaDB 11.8.9, `phoenix-m01-fix-validation`):
  `TestHistoryClear_WatermarkFencesReplay/{sqlite,mariadb}`,
  `TestHistoryClear_FenceIsScopedAndWidens/{sqlite,mariadb}`,
  `TestHistoryClear_UnknownMonitorIsNotFound/{sqlite,mariadb}` — all passed.
- `TestHeartbeatService_ClearHistory_UsesAuthorizedStore` — the clear time
  crosses the repository boundary in UTC and the authorized store owns the
  deletion; `TestHeartbeatService_ClearHistory` keeps the legacy fallback.

The watermark tests prove the T36 effect directly: pre-clear evidence replayed
after the clear (both the delayed queue and the old-sequence-space cases) is
dropped with a `history_cleared` receipt and a counted drop; post-clear
evidence flows; a replayed drop is discarded idempotently; a recreated
generation is never fenced by the old one; repeated clears only widen the
fence.

Not run: `scripts/probe_runtime_smoke.py`, Playwright E2E, `bun run check` /
`bun run build` (no frontend files changed), and a browser pass. Local
acceptance does not authorize push, deployment, or a production migration.
