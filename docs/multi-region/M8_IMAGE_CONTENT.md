# M8 — bounded offline image content digests

Continuation after local coordinator checkpoint `0870c3a`. No push, installer,
remote host access, container execution or activation is authorized here.

## Why this bounded slice

The metadata seam binds a trusted index to one platform manifest/config but does
not read layers. The coordinator has no concrete remote executor. The guide also
requires correspondence with an actually built image before installer use. No
Go/distroless/probe build images are cached in this environment; no registry pull
is authorized or performed in this slice. Real built/published image
correspondence remains **open**, not replaced by synthetic fixtures.

This slice separately verifies observed supplied layer bytes against that chain.
It proves cryptographic content binding only: not tar syntax, safe extraction,
a merged rootfs, executable identity, publisher authenticity or runtime version.
No content is retained. A future loader/delivery adapter must acquire immutable
verified bytes or reverify its actual bytes; this receipt cannot authorize reuse
of mutable source paths, daemon results, installation, enrollment or cleanup.

## Frozen shared API

`domain.ProbeImageContentVerification` contains `Selection ProbeImageSelection`,
`LayerBytes int64`, and `UncompressedBytes int64`. `Valid()` checks shape only.
The pin and selection retain their existing exact metadata meaning.

`ports.ProbeImageContentInputs` contains `Metadata ProbeImageInputs` plus
`Layers []io.ReadCloser` in exact manifest order. All distinct stream ownership
transfers on entry, including invalid input. Close must unblock pending Read;
callers must not concurrently mutate the transferred list. Capture the list,
close every stream once, join cancellation callbacks before return, and never
return a reader or source path.

```go
type ProbeImageContentVerifier interface {
    VerifyImageContent(ctx context.Context, pin domain.ProbeImagePin,
        inputs ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error)
}
func NewProbeImageContentService(v ports.ProbeImageContentVerifier) (*ProbeImageContentService, error)
func (s *ProbeImageContentService) Verify(ctx context.Context, pin domain.ProbeImagePin,
    inputs ports.ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error)
func NewProbeImageContentVerifier() *ProbeImageContentVerifier // provisioning adapter
func OpenProbeImageLayerSource(ctx context.Context, path string) (*os.File, error)
func RunProbeImageContentVerification(ctx context.Context, args []string, out, stderr io.Writer) int
```

The service validates before I/O, consumes/closes inputs on refusal, delegates
ownership once, rechecks canceled context and exact valid result pin, returns a
copy, and allows only bare existing image/context errors plus the three new fixed
content errors. No storage/executor/codec error detail escapes.

## Verification algorithm and fixed bounds

- Reuse the existing metadata parser/helpers and startup contract without changing
  `verify-image`, its domain/port, or its interpretation. Read metadata once;
  verify exact index, selected manifest and config digest/size, JSON limits,
  unique platform and all current config checks before reading any layer.
- Require exactly one supplied stream per ordered descriptor, 1–128 total.
  Validate all declared compressed sizes and encodings before layer I/O.
- Fixed limits: 128 MiB compressed per layer; 512 MiB total compressed;
  512 MiB uncompressed per layer; 2 GiB total uncompressed. Stream incrementally;
  read at most the applicable remaining budget plus one byte to detect overflow.
  Metadata remains at most 4 MiB per blob. Use per-instance test-only smaller
  budgets to prove boundaries without allocating production-sized fixtures.
- Supported content encodings are OCI/Docker uncompressed tar bytes and gzip.
  OCI zstd is metadata-valid but **content-unsupported** here; no dependency is
  added, no success is returned and no layer is read for that unsupported chain.
- Hash all exact compressed bytes, including headers/trailers, compare descriptor
  SHA-256 and exact size. Hash the complete uncompressed byte stream and compare
  its ordered rootfs diff ID. Gzip checksum/truncation/trailing-invalid-data errors
  refuse; joined valid concatenated gzip members contribute to the same diff ID.
  Raw tar bytes are opaque: this does not inspect entries, paths or whiteouts.
- Wrong compressed digest/size uses existing fixed image digest/size errors;
  unsupported/invalid gzip uses `ErrProbeImageContentEncoding`; uncompressed hash
  mismatch uses `ErrProbeImageContentDiffID`; resource budget exhaustion uses
  `ErrProbeImageContentLimit`; raw reader errors become image unavailable.
  Over-budget refusal deliberately takes precedence without completing a too-large
  stream's remaining hash/codec checks. Within budget the refusal order is source
  I/O, compressed size, compressed digest, codec completion, then diff ID.
- Context checks and cancellation-driven closure cover metadata, each layer and
  decompression. No content-sized allocation, output extraction, filesystem write,
  shell, daemon, registry, network, DB, SSH or background worker is used.

## Explicit local command

`phoenix-probe-admin verify-image-content` takes the existing verify-image flags
and one repeated `--layer PATH` per ordered layer. Dispatch occurs before loading
hub config/DB. Reuse the nonblocking regular-file source policy, with the 128 MiB
layer cap and fixed safe errors; close partial opens on every error. The command
uses a 30-second context and rejects missing/excess layer args before opening.

Return an explicit snake_case DTO, never a domain struct. Include existing safe
pin/selection/reference fields plus state `image_content_digests_verified`,
layer byte totals, `layer_digests_verified=true`, `diff_ids_verified=true`,
`rootfs_verified=false`, `runtime_version_verified=false`,
`content_retained=false`, and version_source `digest_bound_config_label`.
It must not advertise installed, rootfs-safe, publisher-verified or runtime-version
proof. Unknown flags, partial opens and paths/reader/codec diagnostics never leak.

## Disjoint implementation ownership

Main owns this doc, new domain/port, test fixtures, adapter + adapter tests,
platform source helpers, early main dispatch, integration/mutations/gates/docs.
Service worker owns ONLY NEW `internal/core/services/probe_image_content.go` and
`probe_image_content_test.go`. CLI worker owns ONLY NEW
`internal/bootstrap/probe_image_content.go`, `probe_image_content_test.go` and
`cmd/phoenix-probe-admin/image_content_test.go`. No shared/existing file changes
by workers. No schema, journal, runner, M7, HTTP, auth, frontend or Helm changes.

## Required acceptance

Exercise real in-memory tar/gzip bytes on OCI and Docker metadata, both arches,
multiple ordered layers, exact compressed and diff hashes, wrong/truncated/extra
bytes, rebound valid metadata with bad diff ID, checksum/trailing/multimember gzip,
all resource boundaries, malformed/unsupported chain before layer reads, private
reader errors, invalid input, cancellation before/mid read and during inflate,
all-stream exactly-once closure and no source mutation/retention. Prove CLI
subprocess dispatch before invalid hub settings, no input path/error leakage, DTO
non-claims, flag refusal and nonblocking FIFO/device refusal where supported.
Service mocks do no DB/HTTP/real I/O. Main independently verifies all worker output.
Use assertion mutants and positive control; compiler errors/panics/timeouts do not
count. Run applicable full build/race/vet/format, Linux CGO-free builds and gate
parser/shard regressions on frozen source. Report skipped/unavailable real-image,
MariaDB, tooling and operational gates honestly. No new DB semantics are changed.

## Verification corrections and inspection

`control-01` retained two test-assumption failures (plus their parent failures)
and one macOS FD-enumeration skip. Compressed layers of identical tar length had
different gzip lengths: swapping them correctly failed size before digest, and
using the first layer's size as the maximum wrongly rejected the larger layer.
`fixture-repro-01` reproduced both assumptions three times.
`fixture-differential-01` changed only test expectations/bounds and passed all
69 named tests; no production verifier change was needed. The final ordering
fixture rebinds swapped sizes while retaining incorrect ordered digests, so
size cannot hide the intended hash-order check. The boundary uses the largest
actual compressed layer. macOS `/dev/fd` enumeration was replaced by a bounded
Fstat check, so FD closure coverage now executes rather than skips.

To isolate cryptographic checks, compressed-digest corruption changes only gzip
MTIME (decoded bytes/checksum unchanged); declared-size corruption keeps valid
compressed bytes and digest. A read-only outsider review considered simpler reuse
and traced the real entrypoint through the unchanged metadata parser/helpers,
service and layer verifier. It found no scoped correctness blocker; it ran no
tests and is supplementary, not acceptance. Main independently reruns worker
code against the real adapter; worker simulated-adapter passes are not counted.

## Evidence and remaining limits

Committed and published as **`c983ec2`** after `0870c3a`; source and docs matched
the evidence before publication, with no commit-only test rerun. Frozen source:
`4c49204147c432e6aca285eab5789fb1a033a75260f9fda8150c3fe6f2f77727`
(1,477 source files). Private ignored evidence is under
`tmp/m8-image-content-verification/`; overlays are dot-prefixed and never alter
original Go source. Final verification and owned-scratch cleanup are complete;
`verification-result.json`, `coverage-audit.json`, `cleanup-result.json` and
`final-audit.json` bind the results to unchanged source contents and inventory.

Executed on cached Go 1.26.9 with offline/sanitized environment (Darwin arm64
host, default/SQLite tests; no MariaDB DSN):

| Gate | Result |
|---|---|
| Image metadata + binary + content race, three repetitions | 918 named passes, including 354 new content passes; zero failures/skips |
| `go build ./...`, `go vet ./...` | Passed |
| Separate CGO-free Linux amd64/arm64 builds | Passed; binaries not executed |
| Parser / repository-shard regressions | 27 / 17 passed |
| `gofmt -l internal/ cmd/`, whitespace | Empty/pass |
| Assertion mutations | 13 caught by intended assertions, no compile/panic/race/timeout substitutes |
| Untouched-source post-mutation race control | 118 named passes; zero failures/skips |
| Full default race, 2400s package budget | Incomplete: repository package timed out; not acceptance |
| Full default race, unchanged source with 3600s budget | Passed: 6,055 named passes, 23 passing packages, zero failures; 547 named skips; 1,893.098 seconds |
| Owned-scratch cleanup | Passed; logs/manifests/overlays retained; no DB/Docker resource created or removed |

`mutations-01` stopped fail-closed on an incorrectly named service-test selector:
no named test executed, so it was not counted as a catch. After correcting only
the private harness selector, `mutations-02` exercised all 13 mutants and a fresh
untouched-source control passed. Original source did not change. Passed static/
focused stages were reused only after matching the frozen contents and inventory.

`full-race-01` exited 1 after 2,537.598 seconds with `panic: test timed out after
40m0s`: 5,865 named passes, no named assertion failures, 434 named skips and 22
passing packages. The active `TestLocalRegionalApplicationContract/sqlite/RecordingFence`
was two seconds old when the whole repository budget expired; no production
deadlock is established. This incomplete run and the earlier selector failure
remain retained. `full-race-02` passed on the unchanged entire tree with a
3600-second package budget; no test, scenario, fence or assertion was removed or
weakened. The shorter wall time on the rerun is not evidence of a source fix.

No DB, credential, container, volume or image is created for this slice. Local
Docker inventory found no relevant cached build inputs; no registry call/pull,
image build/load, SSH or runtime invocation was performed. Tar/gzip fixture bytes
are genuine byte streams but remain synthetic, not built-image correspondence.
The stateless checks do not establish MariaDB acceptance. The full suite had
**542** named skips for missing MariaDB DSN plus five existing optional/helper
cases: `TestDatabaseChecker_Check_MongoDB_RealServer`,
`TestTelegramSender_Send_DownSeverity`, `TestEdgeCheckCrashChild`,
`TestEdgeDiskFullCriticalCommit`, and `TestConfigGroupCycleHelperProcess`.
No unexplained skip reasons or malformed events remained; 14 packages had no test
files. MariaDB-named parent passes do not count as engine execution. No real
MariaDB acceptance was rerun for this stateless slice, which changes no storage
or distributed authority semantics. Earlier journal/runner real-engine records
retain their historical scopes; broad M7 MariaDB acceptance remains open.
`golangci-lint` and `govulncheck` are unavailable; vet/format are fallback evidence,
not completed lint/vulnerability scans. No frontend/Helm source changed, and
those gates were not run. A supporting worker reported an
unidentified temporary code-backup directory outside the repository; its removal
is not claimed. No private source input or credential was supplied to workers.

The subsequent [actual local image gate](M8_IMAGE_CORRESPONDENCE.md) records
separate approved public pulls and source-pinned builds, and catches the
Dockerfile target-argument bug by checking copied ELF bytes. This does not
retroactively expand the historical offline-only evidence above.
The later [private snapshot prerequisite](M8_IMAGE_SNAPSHOT.md) copies and
re-verifies supplied metadata/layers for a context-bound in-process handoff.
It does not change this verifier or its CLI: `content_retained` remains false
for the verification-only command. A retained snapshot is not a complete OCI
layout or remote delivery/installation authority.

The complete systemd/Docker executor,
immutable delivery, host ownership/fencing, joined remote termination/cleanup,
registry/published-artifact correspondence, publisher/runtime proof, async installer API,
authenticated enrollment/exact config activation, broad M7 MariaDB acceptance,
M6/M8 operational/release and deployment approval remain open.
