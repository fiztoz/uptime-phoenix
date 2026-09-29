# M6 — edge ENOSPC recording acceptance (matrix T20 slice)

**Bounded T20 evidence, not the full section-13 run or every M6 kill-point test.** This verifies that a real Linux filesystem-capacity failure during the edge's critical recording transaction is not reported as durable success. No host filesystem was filled and no production database, VM, provider or canary was touched.

## Reproduce

```sh
python3 -B scripts/m6_edge_disk_full.py
GOTOOLCHAIN=go1.26.6 go test -count=1 -run '^TestEdgeHealthStorageUnavailable$' ./cmd/probe
```

The script requires a **local Unix Docker socket** and a *cached* `mariadb:11` image; it fails before starting anything if either precondition is missing. It cross-compiles the `internal/adapters/repository/edge` Go test binary for the container's Linux architecture using the locally installed Go toolchain (`CGO_ENABLED=0`, `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`) so a missing dependency cannot trigger a download. It starts a random-name container with **no network**, no published port, no MariaDB server and a **32 MiB `/data` tmpfs** (512 MiB container memory cap). It copies the binary, runs exactly `TestEdgeDiskFullCriticalCommit` with `TMPDIR=/data`, requires a named `PASS` and no `SKIP`, then removes its own container in `finally`.

The test first verifies that `/data` really is a Linux tmpfs mount; an environment flag alone cannot make it fill an arbitrary host directory. It opens the real CGO-free SQLite edge store on that mount, enrolls and activates a configuration, verifies normal write health and truncates the WAL. A separate file consumes the remaining mount space until the kernel returns **ENOSPC**. With the volume still full:

1. The production `Store.CommitEdgeCheck` of a DOWN observation, incident, transition and provider intent returns the redacted `ErrStorage`, with a **zero** returned sequence (never a successful durable-recording claim).
2. `Store.CheckWritable` returns `ErrStorage` while `ReadDiagnostics` still reads the unchanged progress. `cmd/probe/runtime.go` calls both methods for health; `TestEdgeHealthStorageUnavailable` checks that the production `edgeHealth` projection produces `ready=false`, `db_writable=false`, and `storage_unavailable`.
3. Once the filler is removed, the test **closes and reopens the real edge DB**. Stream high-water remains zero and regional state, telemetry, incident and delivery tables remain empty. `CheckWritable` recovers. Retrying the exact record commits sequence 1, two telemetry events and one provider intent. Another close/reopen verifies durable state, original incident identity, sequence 2 high-water and **one** delivery intent—not an inflated retry.

The check exercises an actual disk-capacity failure on the driver and the production SQLite transaction, not a trigger that merely spells "disk full" or a fake repo that compares instants.

## Executed evidence (2026-09-29 UTC)

- `python3 -B scripts/m6_edge_disk_full.py`: **1 named Linux/tmpfs test pass**, 0 skips, real `modernc.org/sqlite` store. No container left behind.
- `GOTOOLCHAIN=go1.26.6 go test -count=1 -run '^TestEdgeHealthStorageUnavailable$' ./cmd/probe`: **1 pass**.
- `GOTOOLCHAIN=go1.26.6 go build ./...` passed; `go test -race -count=1 -timeout 2400s -p 4 ./...` passed 22 packages; `~/go/bin/golangci-lint run ./...` returned 0 issues. No MariaDB test DSN was configured, so those Go MariaDB legs did not execute.
- The normal Go test suite **skips** `TestEdgeDiskFullCriticalCommit` when the isolated-container flag is absent: JSON-audited focused output was `skip TestEdgeDiskFullCriticalCommit`, `pass TestEdgeHealthStorageUnavailable`. A normal package-level PASS is not T20 evidence. The script requires the exact named pass and refuses a skip. The Linux/tmpfs acceptance is not a `-race` process: it cross-compiles a CGO-free Go test binary for the container.

SHA-256 of tested inputs:

- `scripts/m6_edge_disk_full.py`: `959a522eab641093254a91244d529ef781d219ad6723644d3225f6ed13f341ac`
- `internal/adapters/repository/edge/disk_full_test.go`: `3c8f538a3310101d90e811dd6076f3d7cd85bb46b6ec70260479f91dbd9aba55`
- `cmd/probe/runtime_health_test.go`: `279cb3569046d3057d699f4c5ec4758fcd44a3897b9644bb286b73feb6a8cc3d`

## Limits

This is a Go test binary using the production store and a separate unit check of the health mapping, **not** the compiled `cmd/probe` runtime emitting a health frame or `/readyz` through a real WebSocket/TLS connection. It does not deliver a notification, run a scheduler, assert provider-side effects, simulate a power cut during `fsync`, or fill an operator's real filesystem. There is no hub engine in this edge-only slice. The rest of the section-13 matrix, load/backlog envelopes, production migration gate, remaining kill boundaries and canary stay open.
