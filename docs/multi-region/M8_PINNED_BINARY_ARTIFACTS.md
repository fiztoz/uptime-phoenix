# M8 — pinned probe binary preparation

Continuation after the published asynchronous preflight checkpoint `82822ef`.
This bounded prerequisite prepares a verified **local binary snapshot**, not an
installation or enrollment receipt. The broad M7 MariaDB acceptance blocker in
[M8_ASYNC_PREFLIGHT.md](M8_ASYNC_PREFLIGHT.md) remains open.

## Contract

- Reuse the release pipeline's raw `uptime-phoenix-probe_<version>_linux_<arch>`
  artifact, not an archive, hub binary or mutable `latest` reference.
- The operator supplies an out-of-band trusted SHA-256, exact positive byte size,
  release version without `v` (at most 64 characters), and `amd64` or `arm64`.
  Maximum size: 128 MiB.
  Reading a checksum next to untrusted bytes does not authenticate a publisher;
  this slice deliberately does not fetch or trust a manifest automatically.
- Copy the bounded input stream once to a private 0700 temporary directory / 0600
  file; require exact size and SHA-256. Check the copied bytes, never reopen the
  original input for later use. Freeze the snapshot read-only before returning it.
- Require a little-endian, 64-bit, executable Linux ELF matching the architecture,
  no ELF interpreter, Go build metadata for `github.com/fiztoz/uptime-phoenix/cmd/probe`,
  `GOOS=linux`, matching `GOARCH`, and `CGO_ENABLED=0`.
  The release uses `-trimpath`, which deliberately omits `-ldflags` from Go build
  information. Do not pretend to extract or verify its embedded runtime version
  from that metadata or a substring search. Version-to-digest binding comes from
  the operator's trusted pin; output calls it `pinned_version` with
  `version_source=operator_pin`. Future authenticated runtime enrollment must
  independently confirm the reported agent version. Build metadata is only a
  consistency check; the trusted digest is the byte authority.
- Never execute the artifact to inspect its version. No shell command, network,
  credential, database, registered probe, or remote resource is involved.
- Preparation owns and closes its input on every return. The supplied closer must
  unblock a pending read. Cancellation closes and joins that close callback;
  failures remove the task-owned partial snapshot. Errors are fixed diagnostics,
  never source paths, input contents or underlying filesystem errors.
- A prepared snapshot exposes only an immutable pin, context-bound read handles,
  and explicit close/cleanup. Closing revokes its readers and removes only its
  own directory, checking directory identity before deletion. Local privileged
  filesystem tampering is not defended as a cryptographic trust boundary.
- Future installers must match the pin to fresh host architecture/runtime,
  persist it in their immutable operation/resource journal, verify remote copied
  bytes before execution, and require authenticated enrollment plus exact config
  activation before reporting installation success. A verified local snapshot
  supplies none of that authority.

## Operator surface

`phoenix-probe-admin verify-artifact --file PATH --version VERSION
--architecture amd64|arm64 --sha256 LOWERCASE_HEX --size BYTES`

This local-only command dispatches before loading the hub environment or opening
its database. It reports explicit `artifact_verified` JSON and then disposes its
snapshot. It does not export a reusable pathname or a durable receipt. Read-only
source opening is supported on Linux/macOS; other operator platforms refuse this
command without changing existing operator commands. The preparation and source
close are bounded by a 30-second context, subject to the supplied I/O closer
contract. Cleanup is attempted even after cancellation.

## File ownership

Main owns the new domain/ports/service, provisioning adapter and source opener,
operator bootstrap/dispatch/tests, and documentation. No parallel source owners.
No migrations, new dependencies, HTTP changes, installer actions, or deployment.

## Acceptance

Tests must use genuinely linker-stamped Linux amd64/arm64 Go ELF fixtures (and a
real release probe build for an independent smoke check), not only fabricated
headers. Cover wrong digest/size/version syntax/architecture/component/CGO/ELF,
honest version provenance, blocked-read cancellation, source errors, private copy
stability after original replacement, read revocation, partial cleanup,
replacement-directory cleanup refusal, explicit DTOs, canceled/bad CLI inputs,
and real subprocess dispatch before invalid hub environment. No execution of
verified or rejected artifacts. Full build/race/vet/format gates remain required;
record executed versus skipped evidence separately. Docker digest verification,
remote copy/install/resource journal and real-host installation remain open.

## Executed evidence

Evidence is private/ignored under `tmp/m8-pinned-artifact-verification/`.
Final frozen Go/SQL/Python/shell/module source manifest:
`89fbd3b74703d7c9f69a542d62d44ec4209644daf7e15b713ed5b5ad817d5fdb`.

| Executed check | Result |
| --- | --- |
| Default/SQLite full race suite (`run-02`) | 23 test packages, **5,613 named passes**, zero failures |
| Named skips in that default suite | 502: 497 explicitly missing MariaDB DSN + five existing cases |
| Focused final three-repeat race control (`mutations-02`) | **93 named passes**, zero failures or skips |
| Assertion mutations | All **seven caught** by intended test assertions |
| Build and vet (`./...`) | Passed |
| Strict dual-engine parser regressions | 17 passed |
| gofmt / diff whitespace | Empty / passed |
| CGO-free Linux amd64 and arm64 builds (`./...`) | Both passed |
| Frozen source before/after and final link/import checks | Unchanged; no broken document links or new core framework imports |

The five existing non-MariaDB skips are real MongoDB, Telegram severity,
crash-child helper, isolated disk-full and config-cycle helper cases. Thirteen
package-level `skip` events mean no test files, not skipped named tests. No new
MariaDB instance or credentials were needed for this filesystem-only seam, and
no fresh whole-backend MariaDB acceptance is claimed. The earlier M7 blocker is
unchanged; this work does not modify its source or tests.

Commands used the cached **Go 1.26.9** toolchain, offline module settings
(`GOENV=off GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off`), a durable private `.work`
TMPDIR, and no MariaDB DSN:

```sh
go test -race -count=1 -json -timeout 2400s -p 4 ./...
go build ./...
go vet ./...
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
gofmt -l internal/ cmd/
git diff --check
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...
go test -race -json -count=3 -timeout 8m \
  -run 'TestProbe(Binary|AdminBinary)' ./internal/core/domain \
  ./internal/core/services ./internal/adapters/provisioning \
  ./internal/bootstrap ./cmd/phoenix-probe-admin
```

The seven assertion mutations bypassed digest, size, component metadata,
directory-identity cleanup, reader revocation, exact service pin validation, and
pre-hub command dispatch. Compiler errors, panics, races and whole-test timeouts
were not counted as catches. Overlays were isolated under `.overlays/`; source
was never mutated for the experiments. The initial focused run had 30 named
passes; the final control adds an explicit pre-/mid-parse metadata cancellation
case (31 names, repeated three times). An initial full run was intentionally
interrupted to add those context-aware metadata reads; it is preserved in
`run-01/interrupted.json` and is not counted as acceptance.

Tests built real minimal Go Linux amd64/arm64 ELF fixtures plus the actual current
`cmd/probe` with release flags and a test version stamp. They did not download or
claim publisher authenticity for an official release, and did not execute the
artifacts. The real operator entrypoint was exercised in subprocesses with
invalid hub settings. Success and refusal removed their private snapshots;
replacement-directory tests proved unowned resources survive. The changed
version-label test explicitly proves `pinned_version` is input provenance, not
an invented embedded-version observation.

`golangci-lint` and `govulncheck` remain unavailable locally; vet and formatting
are the fallback, not a full lint/vulnerability gate. Frontend/Helm were untouched.
No VM, SSH mutation, download, Docker image validation, install, enrollment,
configuration activation, live opt-in, release or deployment is established.
Docker digest preparation, durable installer/resource journal and full M8/M6
operational/release acceptance remain open.
