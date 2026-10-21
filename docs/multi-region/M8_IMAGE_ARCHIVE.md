# M8 — closed single-platform image archives

Continuation after local snapshot commit `f1dd181`. This slice constructs a bounded
uncompressed OCI-layout-shaped tar archive from a borrowed private image snapshot.
It is not a Docker-load compatibility claim, complete multiarch export, registry
client, remote delivery/executor, publisher/runtime proof or installation feature.
No existing verifier/snapshot, journal, DB, bootstrap, HTTP or CLI API changes.

## Frozen domain and ports

`domain.ProbeImageArchiveDescriptor` contains only values:

```go
type ProbeImageArchiveDescriptor struct {
    Content ProbeImageContentVerification
    LayoutIndexSHA256 string
    LayoutIndexSize int64
    ArchiveSHA256 string
    ArchiveSize int64
}
```

`Valid()` validates the existing content result, lowercase SHA256 values, positive
index size <= 4 MiB, and 512-byte-aligned tar size >= 1 KiB and <=
`MaxProbeImageArchiveBytes` (512 MiB layer bytes + three 4 MiB metadata budgets +
132 KiB entry overhead + 1 KiB terminators). Shape is not execution evidence.
New fixed errors: `ErrProbeImageArchiveStorage`, `ErrProbeImageArchiveClosed`,
`ErrProbeImageArchiveBusy`, `ErrProbeImageArchiveIntegrity`.

```go
type ProbeImageArchivePacker interface {
    PackImageArchive(ctx context.Context, pin domain.ProbeImagePin,
        image PreparedProbeImage) (PreparedProbeImageArchive, error)
}
type PreparedProbeImageArchive interface {
    Descriptor() domain.ProbeImageArchiveDescriptor
    Open(ctx context.Context) (io.ReadCloser, error)
    Close(ctx context.Context) error
}
```

Service: `NewProbeImageArchiveService(p ports.ProbeImageArchivePacker)
(*ProbeImageArchiveService, error)` and `Pack(ctx, pin, image)
(ports.PreparedProbeImageArchive, error)`.
Adapter: `provisioning.NewProbeImageArchivePacker(baseDir string)
*ProbeImageArchivePacker`; empty baseDir uses OS temp, otherwise trusted local
operator-selected staging. No caller path or arbitrary archive input is accepted.

## Original trust versus generated layout

The original trusted index is NOT substituted with a new single-target index and
then verified using the old pin. Those are different byte identities. Reuse the
existing content verifier against the ORIGINAL index/manifest/config and ordered
layers. Its exact content result must equal the borrowed snapshot's entry receipt.
The service independently requires output Content to equal its entry receipt.

The tar root index is generated separately: schemaVersion 2, OCI image-index media
and exactly one descriptor for the selected original manifest (unchanged digest,
size, media type), platform linux plus pinned architecture. Normalize away unrelated
platforms/annotations. Root layout-index hash/size have DISTINCT descriptor fields;
they may coincide with the original index only when the bytes actually coincide.
The original multi-platform index is not exported as an incomplete loadable root.

Entries are only regular USTAR files: `oci-layout` (imageLayoutVersion 1.0.0),
`index.json`, and `blobs/sha256/<lowercase digest>` for the selected manifest,
config and all compressed layers. Preserve manifest/config/layer bytes and media;
do not rewrite Docker schema-2 into OCI manifest bytes and change its identity.
Identical digest-addressed blobs appear once; ordered repeated layer edges remain
in the unchanged manifest and every supplied ordered stream is still verified.
Refuse inconsistent sizes for a reused digest. No source filenames, links, sparse
files, PAX headers, directories or original/unselected-index blob are emitted.
Headers have deterministic mode 0444, UID/GID 0, empty names and epoch mtime.
Tar header/block padding and two zero terminators are deterministic. No layer tar
is parsed/extracted and no rootfs or copied executable is interpreted.

The standalone archive does not carry the original index inclusion proof. Its
in-process descriptor retains that ORIGINAL verified identity; a future recipient
must obtain/authenticate that evidence or an independently trusted selected digest.
Archive hashing is not publisher authentication. Importer support for an OCI index
which preserves a Docker media-type child is deliberately unverified here.

## Borrowing, streaming, private lifetime

Snapshot ownership NEVER transfers: never call image.Close, including invalid
requests, failures and cancellation. Validate ctx/pin/typed nil before methods;
observe valid exact entry Verification before Open. Borrow one Open bundle and
own/close every returned distinct stream, even partial/error results; typed nil
must not dispatch methods, equality-detectable duplicates close once. Capture the
ordered layer list. Join all source cancellation callbacks/closures before return.
The caller can reopen/close its snapshot after packing; caller closes it eventually.

Reuse existing metadata parser/digest helpers to read bounded original metadata,
then the existing content verifier with metadata replay and stream-to-tar layer
wrappers. No second layer-sized temporary snapshot is needed. Stream each bounded
layer into a fixed tar header while the verifier hashes/decodes exactly that
source. Clip any refusal size+1 sentinel from persistent output. A duplicate blob
still consumes/verifies its stream but writes no second entry. Refuse any tar/write
error with a fixed storage error, even if the reused verifier sanitizes that I/O
error. Do not mask confirmed failed cleanup behind cancellation.

Output is a private 0700 directory with one generated exclusive 0600 file. After
tar completion and Sync it is sealed 0400; close the writer before readback. Enforce the complete archive
budget while writing, then read back the sealed file and independently compare
exact size and whole-file SHA256 to the observed write digest before publishing.
One context-bound reader at a time; close/cancel releases the reader, allowing
reopen. Archive Close revokes and joins readers then guarded owned cleanup, even
with nil/canceled cleanup ctx. Reuse existing snapshot storage/reader cleanup
helpers where possible WITHOUT modifying their verified implementation.

Trusted local staging/same-UID/root threat model matches the snapshot contract.
Check directory/file identity/type/permissions/size. Refuse observed replacement
or unknown contents before deleting anything; no recursive janitor. Preserve
foreign data. Cleanup may be retried through a returned handle. Failed preparation
returns no handle: preserved storage refusal needs trusted staging inspection.
No path, raw reader/codec/tar/storage diagnostic or mutable byte slice escapes.

## Service error and acknowledgement contract

Service validates before adapter I/O, never closes borrowed image, checks caller
context after entry Verification, pack and Descriptor access, validates descriptor
and EXACT Content equality, and disposes every partial/refused output independently
of cancellation. Bare error allowlist: eight existing image errors, three snapshot
errors, four new archive errors and two context sentinels. Unknown/private errors
become ErrProbeImageUnavailable. A failed output cleanup returns bare archive
storage; archive or snapshot storage causes survive simultaneous cancellation.
No service filesystem/network access; tests use mocked ports only.

## Disjoint implementation ownership

- Main: domain + domain tests + ports, frozen contract, independent real-adapter
  integration tests, docs, verification and all final integration decisions.
- Grok 4.7 service worker: ONLY NEW `internal/core/services/probe_image_archive.go`
  and `probe_image_archive_test.go`.
- The adapter worker was assigned ONLY NEW
  `internal/adapters/provisioning/image_archive.go` and `image_archive_test.go`.
  Delegation timed out after the production file existed, before its test file
  existed. After cancellation joined, main took over integration and fixes and
  wrote `image_archive_integration_test.go` and `image_archive_limits_test.go`.
- Delegations requested `grok-4.7` for planning, implementation and read-only
  review. Serving-model metadata was not independently available; requested
  selection and actual serving identity are not conflated.
- Do not modify existing files/helpers/verifiers/snapshots/runner/journal/store,
  migrations, bootstrap, HTTP, CLI, dependencies, Make/CI, frontend or Helm.
- Workers do not execute gates, spawn, read credentials/home, network or mutate git.
  Main independently executes all controls; reports/mocks are not acceptance.

## EOF seam correction before acceptance

**Summary and symptom.** The first real service→packer controls returned fixed
`ErrProbeImageUnavailable` for valid filesystem-backed snapshots. The byte-stream
differential succeeded. This was found in the uncommitted implementation, not in
a deployed installer or existing release.

**Root cause.** `archiveLayerReader.Read` closed its underlying source at EOF.
The existing content verifier then drained the compressed reader again. A
`bytes.Reader` mock remained readable after its counter-only Close; an actual
snapshot file was already closed and returned `ErrProbeImageSnapshotClosed`.
The verifier correctly sanitized that source I/O failure to unavailable. Platform,
schema, tar identity and pin changes did not explain the shared seam failure.
No debugger was installed; source tracing plus the one-knob file-versus-byte
reader differential isolated it. `eof-repro-01` reproduced file-backed failure
three times while the byte differential passed three times.

**Fix.** Cache EOF before closing the underlying source, so subsequent reads
return EOF without accessing the closed file. Unexpected `image.Open` EOF is
also converted to the bare unavailable sentinel, not passed through as a public
error outside the allowlist. No existing verifier/snapshot file was changed.

**Validation and gap.** `TestProbeImageArchiveLayerEOFLifecycle` now passes with
a real `os.File`; real service/snapshot/archive controls pass for both declared
architectures and both schemas/encodings. The gap was mock-only lifecycle
coverage, not a need to weaken digest or codec checks. Retained `control-01`
(89 passes, 23 named/package failure events), `eof-repro-01` (three passes,
ten named/package failure events), `control-02` (339 passes) and expanded
`control-03` (357 passes) distinguish the reproduction from acceptance.

## Executed acceptance and evidence

Frozen source/inventory SHA256:
`dd430a425e4e1fbafe384c84ab8cb0109a0cc8c70c9886359080aec3b46e921f`
(**1,494 files**, baseline `f1dd181`). The task-owned clean mirror copied every
tracked/nonignored project file, proved frozen source bytes identical, used the
exact backend-CI embed placeholder and private GOCACHE. No application package,
scenario, assertion or fence was excluded. Ignored dependency/tool trees are not
part of that CI source surface; shared caches and dependencies were untouched.

| Executed stage | Result |
|---|---|
| `go build ./...` | Passed |
| `go vet ./...` | Passed |
| Five-package artifact race, three repetitions | **1,611 named passes**, including **357 archive**, zero failures/skips |
| Full `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | **6,286 named passes**, including **119 archive**, 23 passing test packages, zero failures; **1,891.043 s** |
| Full skip/inventory audit | **547 = 542 missing MariaDB DSN + five known optional/helper skips**; zero unknown skips/malformed events; 13 no-test packages, all 36 application packages accounted for |
| Linux CGO-free amd64 and arm64 probe/admin cross-builds | Passed, not executed |
| Unchanged Make harnesses | 24 correspondence, four release, 27 dual-engine-parser and 17 repository-shard tests passed, plus existing release controls |
| Assertion mutants | All **13** failed the intended assertion; untouched **119-pass** control, zero failures/skips |
| Formatting/whitespace and local documentation checks | Passed |

Actual service→packer tests independently parse the complete unique tar graph,
verify bytes with both original and separately generated index identities,
check exact deterministic headers/terminators, preserve manifest/config/layers
and deduplicate storage without skipping ordered repeated-stream verification.
They cover OCI/Docker, raw/gzip, both declared architectures, maximum 128-layer
edges, exact/one-over archive budgets, size/digest/codec/diff-ID/receipt refusal,
metadata-before-layer reads, partial Open closure, typed nil/duplicate handles,
private readback corruption, borrowed-input usability, joined cancellation and
late source-close cancellation, automatic reader revocation, one reader/reopen,
concurrent disposal, symlink/file/directory replacement and unknown-file
preservation with cleanup retry.

The 13 mutants cover sticky EOF, selected platform, unique blobs, sealed readback,
index provenance, single-reader admission, cleanup-over-cancellation priority,
exact service receipts, partial output disposal, archive budget, borrowed
ownership, canonical headers and Open-error sanitization. Compile errors, panics,
races, timeouts and zero-test selections did not substitute for assertion catches.
A bounded read-only reviewer found no concrete scoped blocker; its inspection
and unexecuted claims are not acceptance.

Private evidence is retained under `tmp/m8-image-archive-verification/`, including
source/docs/config manifests, stage JSON/stdout/stderr, mirror provenance,
coverage/mutation results, retained failed repros and frozen source copies.
Only verified task-owned `.work` is eligible for cleanup after joined users and
identity checks; evidence remains. No DB/Docker resource was created for this slice.

Native filesystem runtime was **Darwin/arm64**, not Linux. `golangci-lint` and
`govulncheck` were unavailable; vet/formatting do not claim completed lint or
vulnerability scanning. Unchanged frontend/Helm gates were not run. No new
MariaDB acceptance is inferred from default DSN skips. Whole-backend M7 MariaDB,
M6/M8 operational/release gates, importer compatibility, actual verified delivery,
host authority/fencing/cleanup, joined remote termination, installer wiring,
authenticated enrollment and exact config activation remain open.

This verified archive checkpoint was subsequently committed locally as `b9cb6ed`
without changing its verified source/docs/config or rerunning tests merely for the
commit. Private `committed.json` records that exact checkpoint. The subsequent
[read-only binary staging inspection](M8_BINARY_STAGE_INSPECTION.md) is a separate
observation protocol; it neither changes these verified archive bytes nor expands
this evidence into host authority or delivery. No push, image load/run/pull/publish,
remote action, deployment or opt-in.
