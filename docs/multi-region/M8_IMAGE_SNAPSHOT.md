# M8 — private verified image snapshots

Continuation after locally committed `326ceda`. This bounded internal prerequisite
retains the original pinned index, selected manifest/config and ordered compressed
layers in an operation-private local snapshot. It reuses the existing content
verifier; it is not a registry client, OCI exporter, daemon loader, SSH transport,
remote executor, publisher/runtime attestation or installation-success feature.
No endpoint/startup wiring, database, external dependency or live opt-in is added.

## Frozen interfaces

New `internal/core/ports/probe_image_snapshot.go`:

```go
type ProbeImageSnapshotPreparer interface {
    PrepareImageSnapshot(ctx context.Context, pin domain.ProbeImagePin,
        inputs ProbeImageContentInputs) (PreparedProbeImage, error)
}
type PreparedProbeImage interface {
    Verification() domain.ProbeImageContentVerification
    Open(ctx context.Context) (ProbeImageContentInputs, error)
    Close(ctx context.Context) error
}
```

New service:

```go
func NewProbeImageSnapshotService(p ports.ProbeImageSnapshotPreparer) (*ProbeImageSnapshotService, error)
func (s *ProbeImageSnapshotService) Prepare(ctx context.Context,
    pin domain.ProbeImagePin, inputs ports.ProbeImageContentInputs) (ports.PreparedProbeImage, error)
```

Adapter: `provisioning.NewProbeImageSnapshotPreparer(baseDir string)`; empty uses
OS temporary storage, otherwise baseDir is a trusted operator-selected staging
parent, never an HTTP input or an artifact source path. `Verification()` returns
a value, with the original exact pin and existing content verification fields.
No local file path or mutable byte slice is exposed.

## Input and storage ownership

Capture the layer list at entry. All supplied streams transfer on entry even on
invalid/canceled input; callers must supply distinct handles whose Close unblocks
Read and must not mutate the list concurrently. Comparable duplicate handles are
refused and closed once. Typed-nil streams are refused without calling their
methods. Non-comparable reader values must represent distinct handles; they are
closed per supplied slot. Service refusal closes inputs; accepted requests transfer
ownership once to the preparer. Callbacks and input closure join before returning;
cancellation during Close revokes success.

The adapter creates a private 0700 directory and lazy exclusive 0600 regular
files with fixed generated names. No caller path/filename is interpreted. The
existing verifier validates the complete declared metadata/platform/encoding/
layer-budget chain before any layer read. Tee readers copy only the exact bytes
observed under its bounds. After this intake verification, sync and seal each
file 0400, close writers, then **re-run the existing verifier over the private
copies**. Only that second successful verification may produce a snapshot.
Metadata remains at most 4 MiB per file; compressed layers remain bounded by
128 MiB/layer and 512 MiB total. A refused over-size stream may require a one-byte
read sentinel, never an unbounded disk copy. Inflate limits remain 512 MiB/layer,
2 GiB total per verification; two bounded passes are intentional. Tar/rootfs is
not interpreted, layers are not combined or extracted, no binary is executed.

Open uses a descriptor-rooted directory and verifies directory/file identities,
regular type, read-only permissions and size. No source path is reopened. A
snapshot allows **one active bundle** at a time (at most 131 readers); after every
reader closes, a new Open may obtain an independent bundle. Partial-open failures
close all newly opened handles. Each reader follows its Open context. Snapshot
Close revokes and joins all outstanding reader closures, including callbacks,
before cleanup; it releases resources even with nil/already-canceled cleanup
context. Close is idempotent and serialized; failed cleanup can be retried.

Staging is trusted local storage, not a security boundary against another process
with the same UID/root modifying the directory concurrently. Refuse observed
replacement directories/files/symlinks and unexpected contents; never recursively
delete a replacement or unknown file. Remove only fixed owned files through the
descriptor root and then the empty owned directory. Preserve/refuse a mismatch,
return a fixed storage error, and do not claim successful disposal. Confirmed
cleanup failure takes precedence over cancellation at both adapter and service
boundaries. A returned handle supports cleanup retry; a failed prepare returns
no handle, so a preserved storage-failure directory requires trusted staging
inspection, not an automatic recursive janitor. The snapshot is an in-process
lifetime object, not a durable resource journal or cleanup token.

## Service boundaries and errors

Validate ctx, pin, metadata streams and layer count/nils before I/O. Copy layers
before delegation. Refuse malformed/foreign verification (including exact pin)
and dispose any partial or refused snapshot with an independent cleanup context.
Check caller cancellation after preparation and verification access. All private
wrapped diagnostics become bare fixed errors. Allow only the existing eight image
errors, context cancellation/deadline, and new:

- `ErrProbeImageSnapshotStorage`: private snapshot storage unavailable.
- `ErrProbeImageSnapshotClosed`: snapshot/read capability has been revoked.
- `ErrProbeImageSnapshotBusy`: another bundle is still active.

New domain errors are pure sentinels only; existing domain/ports/verifier APIs,
byte-verification semantics and CLI non-claims remain unchanged. Verification
shape is not independent proof of adapter execution. Future production wiring
must use the real adapter; no runtime caller is wired here, and service mocks do
not establish filesystem/content acceptance.

## Implementation ownership

Main owns this contract, domain/ports, new adapter and actual-adapter tests,
documentation and executed verification. A service worker owns ONLY NEW
`internal/core/services/probe_image_snapshot.go` and
`internal/core/services/probe_image_snapshot_test.go`. Do not modify existing
binary/content/image verifiers, services, journal/runner/store, migrations,
bootstrap, HTTP, CI or shared helper files inside that worker slice.

## Required verification and non-claims

Test actual raw/gzip OCI and Docker schema-2 fixtures on both architectures, exact
metadata/layer copy/order and readback, source mutation independence, metadata
rejection before layer reads, all existing digest/size/codec/diff-ID/budget refusals,
private mode/no path exposure, all input ownership and cancel-during-Close cases,
active-bundle bounds, independent reopen, partial-open cleanup, context revocation,
concurrent Open/Close, file/directory replacement refusal, no recursive/foreign
cleanup, cleanup retry and fixed error secrecy. Service tests use mocked ports
only and cover partial-result disposal, exact pin, invalid result, post-port
cancellation, defensive layer copies and bare error allowlists.

Run race controls/repeats, intended assertion mutants plus untouched control,
full Go build/vet/race/format and Linux CGO-free builds on frozen sources. Account
for default MariaDB skips; this stateless slice does not change storage authority
and does not claim new real-engine acceptance. No registry/Docker/SSH network or
mutation is needed. Snapshots do not create a complete multi-platform OCI layout:
the original index may reference unsupplied platforms. A future transport must
specify its exact load/pull protocol and verify actual delivery, not treat this
bundle or a reopened path as remote ownership/installation authority.

## Executed evidence

Private ignored evidence: `tmp/m8-image-snapshot-verification/`. Frozen source
`192038d81e3613c70433fb3c26a3e0b00fe9173f78df712e3dfabb3f62079470`
covers 1,486 source files at baseline `326ceda`; docs/config are audited separately.
Main executed the gates using installed Go 1.26.9, offline/sanitized child
environments and no inherited DSN. The native filesystem OS was Darwin/arm64.

| Gate | Executed result |
|---|---|
| `go build ./...` and `go vet ./...` | Passed, including a fresh task-owned cache/mirror control |
| Artifact race suite, `-count=3` | 1,254 named passes across five packages, including 336 snapshot passes; zero failures/skips |
| Full `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | 6,167 named passes, 23 passing test packages, zero failures; 1,950.661 seconds |
| CGO-free Linux builds of probe/admin | amd64 and arm64 passed; binaries not executed |
| Intended assertion mutants | Nine caught; untouched-source snapshot control 112 named passes, zero failures/skips |
| Existing Make harness regressions | 24 image-gate / 4 release-gate / 27 dual-engine-parser / 17 repository-shard tests passed |
| Formatting and whitespace | `gofmt -l internal cmd` empty; `git diff --check` passed |

The final full command ran from a clean mirror of **every tracked/nonignored
project file**, proving identical frozen source contents/inventory, with the
exact backend-CI embed placeholder and an independent task-owned `GOCACHE`.
This avoids ambient ignored Go sources, not application scenarios. All 36
application packages were accounted for: 23 with passing tests and 13 without
test files. Existing Go test registration includes this slice automatically;
no CI/Make change was needed. The native suite exercised applicable SQLite paths;
**547 named skips** were audited as 542 missing MariaDB DSN cases plus the five
known optional/helper cases, with zero unknown skips/malformed JSON. No new
MariaDB acceptance is claimed. Linux cross-builds do not establish Linux
filesystem-runtime behavior; that execution remains unverified here.

The nine mutants independently cover copied-byte re-verification, single-bundle
admission, reader revocation, whole unknown-graph refusal, directory identity,
automatic cancellation callback completion, cleanup/cancellation precedence,
service exact-pin validation and partial-result disposal. Each failed its
intended assertion, with no compilation, panic, data race or process-timeout
substitution. Supporting worker/reviewer reports were not accepted as execution.
A proposed UnsafePointer nil-handle finding was independently falsified by the
compiler: Go rejects methods on named unsafe.Pointer receivers, so such a value
cannot implement `io.ReadCloser`; no frozen source change was warranted.

### Corrections and retained unsuccessful attempts

- Nested interface values exposed an ownership-map panic: a struct type can be
  statically comparable while an interface field holds a slice, making the
  actual value unhashable. Real service→adapter reproduction and a pointer-value
  differential isolated the guard. `reflect.Value.Comparable` replaces
  `Type.Comparable`; real-adapter regressions cover both field shapes. Mock-only
  pointer handles had not exercised this path.
- Cancellation initially overwrote confirmed failed cleanup. A source-close hook
  created an unexpected staging file and canceled; both boundaries returned only
  `context.Canceled`, hiding preserved storage without a handle. After an
  adapter-only correction, the service differential still failed. Both layers now
  preserve bare `ErrProbeImageSnapshotStorage`; ordinary cancellation still works.
- `artifact-control-02` is incomplete, not passing: null exit and a roughly 1 MiB
  buffered stdout cutoff. The same spawn primitive independently reproduced
  `ENOBUFS`; original spawn-error metadata was not retained. File-descriptor-backed
  capture completed the untouched-source controls. No active users remained from
  the incomplete attempt.
- Direct `full-race-01` failed compilation: 863 named passes, no named assertion
  failures, 15 failed package events and one skip. Compiler/linker/vet reported
  missing shared build-cache archives. The scan also included an ignored
  `web/node_modules/flatted` Go package; the removal time/cause of missing cache
  files was not established. Retained logs are not passing evidence. The clean
  mirror/private-cache rerun above changed no application source, scenario, fence,
  assertion or timeout. No shared cache/dependency tree was deleted to obtain green.

`golangci-lint` and `govulncheck` were unavailable; vet/format are fallback evidence,
not completed lint/vulnerability scans. Frontend/Helm sources were unchanged and
those gates were not run. Only task-owned private compiler/cache/mirror scratch
is eligible for cleanup after joined users and identity checks; logs, mutations,
reproductions and manifests are retained. No DB/Docker resource was created for
this slice. This verified checkpoint was subsequently committed locally as
`f1dd181` without changing its verified source/docs/config or rerunning tests merely
for the commit. Private `committed.json` binds that checkpoint to this historical
evidence. No push/deployment/opt-in. The later [closed single-platform archive](M8_IMAGE_ARCHIVE.md)
borrows this seam; it does not change the snapshot implementation or retroactively
expand snapshot evidence into importer, publisher/runtime or delivery proof.

Registry/publisher/runtime proof, concrete verified delivery, host fencing/
ownership and joined remote termination, installer API, authenticated enrollment
plus exact successful config activation, broad M7 MariaDB and M6/M8 operational/
release/deployment approval remain open.
