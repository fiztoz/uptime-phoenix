# Hub stream-reset retrospective

Date: 2026-09-21. Engineering record for the hub/CLI checkpoint after `ba997db`.
Final acceptance is tracked separately in [the ledger](M3_HUB_RESET_ACCEPTANCE.md).

## Codex implementation and test corrections

The first effect tests failed preparation on both SQLite and MariaDB with the
redacted `probe connection storage failed` error. The draft shared a query across
credential and certificate rotation tables, but only certificate rotation has
`overlap_closed`. The query referenced a nonexistent credential column before
reserving the reset. Schema inspection identified the mismatch. The corrected
query uses the credential's immutable deadline and the certificate's deadline/
closed bit. Both-engine lifecycle and unresolved-rotation tests now exercise this
path with real protected credentials. Similar names do not prove matching schemas.

The next test assumed local cancellation would be exposed as `ProbeCommand.Outcome`.
That field intentionally means a durable source receipt and is only populated
when `RemoteConfirmed` is true. Preserve that meaning. Reset now uses local
`canceled` status and a separate `LocalCancellationCode`; the CLI explicitly says
the old source outcome is unknown and no retry will target the new stream. The
effect test checks the retained ciphertext/stream, absent remote outcome and
separate cancellation reason. Do not weaken truthful receipt semantics to make an
assertion pass.

The first compiled reset run passed its existing replay workflow and hub
preparation, then failed opening the source for exclusive recovery. The Python
harness used `with sqlite3.connect(...)`, which manages transactions but does not
close the connection. Retained diagnostic readers prevented the deliberate
exclusive SQLite lock. With the harness terminated, repeating the same compiled
reset and plan succeeded. Diagnostic reads now use `contextlib.closing` and the
full scenario is rerun from a fresh database. Do not weaken exclusive recovery
locking to accommodate a test harness's leaked database handles.

The second run advanced through source restart, then its own archive inspection
created `edge.db-wal` and `edge.db-shm`. SQLite `mode=ro` may create sidecars for a
WAL database. The source correctly rejected the changed archive layout on retry.
The harness now adds `immutable=1` when reading a published archive, matching the
production verifier, and uses a fresh database for the next acceptance run.
Archive verification was not weakened and the altered failed-run artifacts were
kept as evidence. Read-only SQL does not necessarily imply a read-only filesystem.

The first full repository gate exposed an older migration rehearsal that dropped
and recreated migration 056 while leaving migration 064 recorded as applied.
Because 064 adds `probe_missing_state.reason`, reconstructing only 056 removed a
column required by current code. SQLite's upgrade case failed; the reused MariaDB
test schema then caused cascading current-state/history failures. The owned test
process was stopped after preserving the failures. The rehearsal now removes 064
before 056 and reapplies them in dependency order; the rerun uses a fresh test
schema. The focused both-engine regression and final full gate both passed;
the third compiled process run passed all 30 stages. This is test-fixture
schema drift, not evidence that production may run current code at migration 056.

## Antigravity review and teaching

Conversation `99e617f4-edb4-4190-a451-e7d7e0714329` completed a read-only audit and
a corrective follow-up. It edited no files and ran no tests. Its three initial
claims were not accepted:

- Its proposed admission deadlock contradicted its own generation trace. Current
  credential confirmation accepts matching metadata without requiring a rotation.
- It confused command issuance (`lockCommandTarget`, which blocks every incomplete
  reset) with admitted session work (`lockProbeSession`, which permits awaiting-peer
  confirmation and replay).
- It omitted `withRuntime`'s renewal goroutine. A revoked parent epoch causes
  renewal to fail and cancel the owned context, unwinding the inner reconnect loop.

Codex supplied the exact callers and cancellation path. Antigravity explicitly
retracted all three claims and acknowledged the lesson: trace complete authority
and cancellation scopes before calling a suspected defect proven. Its remaining
generic contention question had no demonstrated failure path and is not an
accepted finding. These corrections do not substitute for executed tests.

## Prevention

Build administrative recovery around an immutable operation and retained evidence.
Assert database effects and rollback under real engine behavior, not only return
codes. Separate operator receipt, local cancellation and authenticated peer
confirmation. Reopen through production startup and CLI paths, and make diagnostic
resource lifetimes explicit. Keep advisory findings provisional until their stated
trigger reaches the claimed effect in the actual implementation.
