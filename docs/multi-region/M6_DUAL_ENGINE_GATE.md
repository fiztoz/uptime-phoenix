# M6 — fresh dual-engine backend race gate (bounded regression evidence)

**Not the full section-13 matrix or release approval.** This runs all Go packages with race detection against a *new* disposable MariaDB 11 test database, confirms that named MariaDB subtests actually passed, and audits every skip. It complements the isolated [T06 process-kill](M6_EDGE_COMMIT_CRASH.md) and [T20 ENOSPC](M6_DISK_FULL_ACCEPTANCE.md) Linux gate.

## Reproduce

```sh
python3 -B scripts/m6_dual_engine_gate.py
python3 -B scripts/m6_edge_disk_full.py
```

The first script refuses a remote Docker socket or an uncached image, starts a random-name container with a freshly initialized `phoenix_ci` database on a random **127.0.0.1-only** port, and removes the container on exit. It does not read or pass through the caller's DB/provider credentials or `TEST_MARIADB_DSN`. The local Go 1.26.6 toolchain, Go modules and Docker image must already be cached: `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`. The container runs MariaDB (not the shared local DB); the Go tests modify and discard **only** this newly created database.

The gate runs `go test -race -count=1 -json -timeout 2400s -p 4 ./...` and checks the process exit, malformed JSON, an audited set of optional/guarded skips, 300+ **named** MariaDB passes, zero MariaDB skips, and exact passes for both T06 crash points and the MariaDB-specific T34 UTC-bound case. It prints only a redacted summary of counts and names, not the DSN or raw test logs. It does **not** quietly convert skipped MariaDB cases into success. T20 is deliberately skipped in the ordinary suite; its separate script requires its named pass on a real Linux tmpfs.

## Executed evidence (2026-09-29 UTC)

- `python3 -B scripts/m6_dual_engine_gate.py`: **exit 0**, 1,034.1 s, **22 Go packages passed**, 4,295 named parent/subtest PASS events, **0 failures**, **340 named `/mariadb` PASS events**, 0 MariaDB skips, 0 malformed events. Exact T06 crash points and MariaDB T34 marker passed; no unknown skip. A first manual run on a different newly created disposable container also passed (22 packages, 4,295 named passes, 340 MariaDB passes, four skips); the scripted run is the reproducible gate.
- Audited skips: `TestDatabaseChecker_Check_MongoDB_RealServer` (no external MongoDB), `TestTelegramSender_Send_DownSeverity` (optional external provider), `TestEdgeCheckCrashChild` (helper run/killed by its parent), `TestEdgeDiskFullCriticalCommit` (requires guarded tmpfs). The second script ran T20 and T06 on Linux and reported 4 named passes, zero skips.
- `GOTOOLCHAIN=go1.26.6 go build ./...`: passed. `GOTOOLCHAIN=go1.26.6 ~/go/bin/golangci-lint run ./...`: zero issues after import formatting. No frontend, Helm, production host or canary changed.

SHA-256 of reproducible gate inputs:

- `scripts/m6_dual_engine_gate.py`: `64b6cd95ea3bf05eef18cab066d243f7ace951ef5a9d4f27625b277e997018e0`
- `scripts/m6_edge_disk_full.py`: `54b47f1d0ee63db7c722a4237c1e2ac0ef3b4ed2f622f4d6dfd51a14ea335654`
- `internal/adapters/repository/edge/crash_commit_test.go`: `56fda974246eba0b5d42184a2ea30190f99da166994501c4c4b2b4ea3a74f6cc`

## Limits

The Go count includes parent tests and subtests; it is **not** 4,295 distinct scenarios nor proof that every T01–T40 assertion passed. A fresh empty MariaDB database is not a representative populated production-sized migration rehearsal. The full 40-scenario matrix still needs a row-by-row test/process evidence map and one coordinated acceptance run. Load envelopes, 24-hour backlog drain, remaining kill/restart boundaries, real probe health-frame acceptance, and an operator-approved canary are unverified. The test container has no production data; its Docker bridge is not an air-gapped network (the published database listener is restricted to localhost).
