# M3 hub watchdog source persistence acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

This increment follows edge persistence commit `c736383`. It adds the hub source
counterpart, protected against stale runtime/session authority and edge identity
adoption. It does not complete or enable either running watchdog. M3 is incomplete.

## Implemented source and ownership

Hub migration 059 adds a durable ownership registry for hub connection incidents,
an immutable transition journal and a per-probe checkpoint. Hub journal sequences
are independent of the edge stream. Source writes never advance remote replay
cursors or create monitor observations, heartbeats, current health or legacy alerts.

The source store checks enabled remote registration, runtime owner/epoch/DB lease,
installation/key, current nonretired stream and applied configuration. New health
additionally checks the child generation/owner/lease. The final DB-clock check
requires the transaction to finish before both applicable lease deadlines. A local
loss tick remains possible during reconnect backoff under the parent alone.

One serializable transaction commits ownership, incident, exact transition bytes,
checkpoint/sequence and delivery intents. Shared lifecycle validation preserves ACK
metadata, rejects reopening old incidents and prevents duplicate recovery work.
The adapter normalizes source incident timestamps to UTC microseconds before both
persistence and encoding. Restart through a separate DB connection retains source
identity and ACK state. Duplicate queued delivery IDs return a typed conflict and
leave all source effects unchanged.

The ownership registry persists for resolved incidents too. Generic mirror APIs
cannot update a hub-owned incident or invent its provider outcomes. The hub source
cannot adopt an edge-owned incident UUID. Edge-owned watchdogs remain valid mirror
entities; banning all `probe_connection` scope would break the two-sided design.
Actual wire watchdog replay authorization is still pending.

The existing durable outbox now represents hub watchdog work with null monitor,
assignment and stream fields, explicit `probe_connection` event kind and the hub
journal source sequence. Claiming requires a hub-owned incident and enabled probe.
Existing lease/retry/outcome storage is reused. A claim alone is not permission to
send: the future dispatcher must recheck configuration, exact lifecycle and runtime
authority immediately before provider I/O.

## Migration and test evidence

SQLite migration DDL/copy/bookkeeping runs transactionally. A late injected failure
leaves no replacement/ownership/checkpoint tables and preserves the original data.
MariaDB uses a resumable copy and atomic table rename while writers are stopped.
Tests interrupt before rename and rerun after rename, preserving all original
availability delivery context and in-flight lease fields. Downgrade rejects any
watchdog checkpoint, journal, ownership or queued work rather than discarding it.

Real SQLite/MariaDB race tests cover source rollback at each participating table,
competing writers, stale ownership/generation/configuration, disabled registration,
wrong installation key, restart/ACK/backward-clock recovery, ownership in both
directions, null source scope, no remote cursor effects and expiry during encoding.
They also cover precision and collision regressions identified by Antigravity.

The final full race suite passed in 21 test packages with 226 MariaDB-named
pass events and zero MariaDB skips. CGO-free build and zero-issue lint passed.
The committed Go/SQL files match the 15 source hashes frozen before that gate.

Exact full-gate results and source hashes are recorded in
[M3_HUB_WATCHDOG_EVIDENCE.json](M3_HUB_WATCHDOG_EVIDENCE.json). The independently
verified failures and review disposition are in
[the retrospective](../postmortems/2026-09-21-m3-integration.md#watchdogs-and-clocks). No new dependency, monitor,
provider, frontend or Helm change is included. No process watchdog acceptance or
actual external provider delivery is claimed here.

## Remaining integration

Keep enabled-watchdog configuration rejected until settings and protected snapshot
dependencies, source service/timer coordination, validated health callbacks, mirror
authorization, probe template context and both-side provider reconciliation run in
the real composition roots. Parent renewal must depend on bounded successful
checkpoint progress. ACK storage is not durable command processing. Commands,
rotation/reset, cleanup, bounded shutdown flushing and the real fifteen-minute
partition remain M3 requirements. No push or deployment was performed.
