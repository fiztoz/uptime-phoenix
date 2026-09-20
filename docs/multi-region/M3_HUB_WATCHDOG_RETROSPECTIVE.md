# Hub watchdog persistence retrospective

## Stored precision disagreed with lifecycle identity

Shared source validation compares immutable start times at microsecond precision,
matching the SQL representation. The initial hub adapter then passed the original
nanoseconds into `putIncidentWithOwnerTx`, whose generic identity comparison used
exact `time.Equal`. A caller retaining its original `time.Now()` timestamp could
open an incident and then fail a valid resolution with `ErrConflict`.

Antigravity identified the mechanism. Codex reproduced it on both real SQLite and
MariaDB with a timestamp 731 ns beyond a microsecond. The hub source now copies and
normalizes `StartedAt`, `AckedAt` and `ResolvedAt` to UTC microseconds before saving
and encoding. The original caller value is not mutated. The regression passes on
both engines. Existing tests missed it because their fixtures already truncated
every timestamp.

## A duplicate pending delivery was reported as an internal failure

The pre-insert identity check covered mirrored outcomes, while the table's primary
key correctly rejected a collision with an existing pending intent. Atomicity was
already sound, but source error sanitization returned `ErrInternal` rather than the
expected typed conflict. It did not expose the raw driver error, contrary to the
initial audit wording.

The source error path now applies the existing unique-constraint classification
before sanitization. Both engines return `ErrConflict`, and the regression compares
the complete before/after checkpoint to prove no effects escaped rollback.

## An older migration test left MariaDB at a stale schema

The initial hub tests passed on fresh SQLite but failed on reused MariaDB. Direct
schema inspection showed `monitor_id`, `assignment_generation` and `stream_id` still
NOT NULL and the event check still limited to the old two event kinds, despite
migration 059 being recorded as applied. The preceding outbox migration test
rebuilt 045/050/051 and stopped there.

The first focused fix restored the outbox after its 045/050/051 migration tests.
The full race gate then exposed the rest of the dependency problem:
`TestRegionalIncidentsAreIndependentPerProbe` attempted to drop 038 while the new
059 ownership tables still referenced its incident table. MariaDB rejected the
parent drop with error 1451 after earlier child tables had already been dropped.
Later fixtures inherited the incomplete schema even though `_migrations` still
reported the latest version. The 035 registry and 047 config tests also needed to
remove 059 before testing their older parent-table boundary.

The affected tests now remove dependents in reverse order and restore 059 after
rebuilding their base schema. A fresh disposable `phoenix_m3_watchdog_ci` database
replaced the damaged test schema for validation. The combined migration, registry,
config, local delivery and watchdog race contracts passed on both engines in
107.818 seconds. The final full-suite result is recorded in the evidence file.
New migration tests verify interrupted copy, retry, lease preservation and SQLite
rollback. This was a test dependency gap introduced by the new schema, not evidence
of an application upgrade failure. A focused package PASS is insufficient if a
fixture leaves shared engine state different from its migration records.

## Authority and review lessons

A health-derived write must finish within the child session lease even if its
runtime parent remains alive. The final fence uses the earlier applicable deadline.
A real-clock test blocks encoding until the child expires and confirms all source
effects roll back on both engines. Local loss ticks require only the still-current
parent, so reconnect backoff does not prevent loss recording.

Antigravity's read-only audit ran in conversation
`b5e7f7af-0f46-4e0a-b9d5-39f805dc2767`. Two other proposed defects were rejected:

- Forbidding all watchdog scope from mirror APIs would suppress legitimate
  edge-owned connection incidents. Separate UUID ownership is the contract. Tests
  prove neither source can adopt the other's existing UUID, including after hub
  resolution.
- The SQLite interruption claim assumed partial DDL persisted outside a
  transaction. The actual runner wraps migration and bookkeeping together. A late
  injected failure confirms rollback; no destructive cleanup workaround was added.

Codex sent the verified outcomes back to Antigravity, distinguishing reproduced
bugs, error classification, unsupported claims and test suggestions. Antigravity
owns no files. Real runtime/provider tests remain necessary before enabling
watchdogs or declaring M3 complete.
