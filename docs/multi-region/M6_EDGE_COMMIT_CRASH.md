# M6 — abrupt edge-commit crash (matrix T06)

This bounded T06 gate kills an OS process at two **real SQLite WAL** boundaries in the edge recording path. It runs locally and again inside a no-network Linux container; it is not an actual VM/K8s probe crash, a power-loss or `fsync` durability test, or the complete section-13 matrix.

## Reproduce

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -json -timeout 180s \
  -run '^TestEdgeCheckCrashAroundCommit$' ./internal/adapters/repository/edge
python3 -B scripts/m6_edge_disk_full.py
```

The second command requires the local cached Docker image and 32 MiB disposable tmpfs described in [the T20 record](M6_DISK_FULL_ACCEPTANCE.md). It now requires T20 plus the parent T06 test **and both named T06 subtests** to PASS with zero skips. The container has no network, external database, or provider credentials and is removed even when a test fails.

In each T06 subtest a child process opens the **production edge Store** on a new parent-owned temporary directory, enrolls, activates config, and records a DOWN observation, regional incident, transition, and provider delivery intent:

- `inside-transaction`: a child-only SQLite function/trigger pauses the final `edge_identity.last_created_seq` update **after** the observation, incident, telemetry and delivery writes but before the SQL transaction commits. The parent waits for an `IN_TX` signal, SIGKILLs the child, reopens the database and asserts zero high-water and zero rows in **all four** affected tables. It then removes the test trigger, retries the same check, and asserts sequence 1, high-water 2. A trigger failure without a process kill would **not** count as this gate.
- `after-commit`: the child sends `COMMITTED` **only after** `Store.CommitEdgeCheck` returns sequence 1. The parent SIGKILLs it before graceful close, reopens the database and asserts high-water 2, one regional state, one incident, one delivery intent and two telemetry events. Repeating the same check must return `ErrStaleLocalState` without duplicating the persisted sequence.

No test-only hook or failure path is added to production code. The SQLite scalar function is registered in the child process only; its trigger is removed after the pre-commit kill before retry.

## Executed evidence (2026-09-29 UTC)

- `GOTOOLCHAIN=go1.26.6 go test -race -json -count=1 -timeout 180s -run '^TestEdgeCheckCrashAroundCommit$' ./internal/adapters/repository/edge`: **parent + two named subtests PASS**, 0 skips on the local host.
- `python3 -B scripts/m6_edge_disk_full.py`: **4 named test/subtest PASS** (T20 + parent T06 + two T06 subtests), 0 skips, on Linux/modernc SQLite with a real tmpfs. No Docker container remains after the run. This runs the T06 test on Linux, not under the race detector; the separate local `-race` run exercises both crash subtests.

SHA-256 of the test inputs:

- `internal/adapters/repository/edge/crash_commit_test.go`: `56fda974246eba0b5d42184a2ea30190f99da166994501c4c4b2b4ea3a74f6cc`
- `scripts/m6_edge_disk_full.py`: `54b47f1d0ee63db7c722a4237c1e2ac0ef3b4ed2f622f4d6dfd51a14ea335654`

## Limits / next gates

The pre-commit stop is deterministic inside the real SQL transaction, not a randomly timed process crash; the post-commit stop is **after** the store method returns, not between SQLite's durable commit and the caller's response. Neither test drives the real probe scheduler, watchdog, hub, a provider, or a node-level power loss. Other required kill points (provider attempt, hub transaction/ACK, config and credential activation, ownership takeover) and the combined dual-engine section-13 run remain open.
