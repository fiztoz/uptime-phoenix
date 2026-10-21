# M8 — exact-stream preparation to fresh local staging

## Frozen scope and reason

Continue after local commit `ce6e106` (inspection plus fresh-only local writer).
Compose the existing binary preparer and staging service to accept an untrusted
raw stream against a separately supplied valid typed systemd intent. A caller
that already has PreparedProbeBinary should keep using Stage; this new boundary
is for raw intake, exact EOF, ownership and pre-mutation completion. No new wire
format, marker parser, identity codec, port, domain errors or filesystem primitive.
No SSH, helper process/CLI, /opt opener, endpoint, runtime/startup wiring, DB call,
current authority/host fence, cleanup of staged resources or installed-success.

The actual existing preparer copies at most pin.Size+1, not pin.Size. This slice
intentionally supplies a bounded child of pin.Size so the receiver retains outer
EOF and source Close responsibility. Synthetic child EOF is NOT whole-input EOF.
No marker or seal is converted to a journal resource outcome or executor grant.

## Main/shared contract

New public service (no new port):

```go
func NewProbeBinaryStageReceiveService(p ports.ProbeBinaryPreparer, w ports.ProbeBinaryStageWriter) (*ProbeBinaryStageReceiveService, error)
func (s *ProbeBinaryStageReceiveService) Receive(ctx context.Context, intent domain.ProbeBinaryStageIntent, input io.ReadCloser) (domain.ProbeBinaryStageSeal, error)
```

Receive owns non-typed-nil input on EVERY return, including refused input, but
borrows the writer and the parent already encapsulated by it. No source path,
parent fd, raw diagnostics or prepared snapshot escapes. Capture intent by value.
Validate nil/typed-nil service, ports, context, input, full intent before preparer
I/O. Refused input still closes once; typed-nil methods are never dispatched.
Input Close must unblock Read. Snapshot close must synchronously revoke/join users
as its existing port requires. No invented hard deadline for arbitrary Close or
kernel calls; caller context revokes input and local stage's own lifetimes.

### Worker A — service + mock tests only

ONLY `internal/core/services/probe_binary_stage_receive.go` and `_test.go`.

1. Validate before I/O (apart from closing refused owned input). Construct the
   source helper below. Delegate PrepareBinary exactly once with exact
   intent.Identity.Binary and helper.Child(). Do not use the older preparation
   facade, whose nil and cleanup checks differ; do not edit that facade.
2. Always close the child after preparer return (idempotent, including a broken
   preparer that forgot to close). Dispose any nonnil/typed-nil-aware partial,
   foreign, malformed or error-bearing prepared result. Require live context,
   exact valid returned Pin, and helper.Complete() before accepting preparation.
   Never Open a prepared binary in the receiver; the stage writer borrows it.
3. ConfirmEOF on the outer input, then Finish to close input and join its
   cancellation callback BEFORE staging. No bytes or handles can still be pending
   at this boundary. A final context check precedes invoking the existing
   ProbeBinaryStageWriteService.Stage once. Unknown/extra/short/no-progress input
   never invokes the writer. No retry, resume or stage deletion.
4. Dispose the prepared snapshot once, using context.WithoutCancel (or Background
   for invalid nil context); Close failure BEFORE Stage is ErrProbeArtifactStorage,
   outranking cancellation. AFTER Stage invocation any snapshot cleanup failure,
   caller cancellation, foreign/malformed/error-bearing seal or private failure
   is zero seal / ErrProbeBinaryStageWriteUnknown. Do not hide effects behind a
   context error. Live-caller explicit stage refusals follow existing Stage
   service allowlist. The only successful result is its exact valid intent seal
   after snapshot cleanup. A later caller cancellation revokes success as Unknown.

Before Stage, permitted errors are the four existing stage-write errors, six
artifact errors, and context.Canceled/DeadlineExceeded, always bare sentinels.
Unknown prep/input errors map to ErrProbeArtifactStorage. Input syntax/refusal is
ErrProbeBinaryStageWriteInput; short/extra/incomplete payload is ArtifactSize;
malformed/foreign prepared pin is ArtifactMetadata. ArtifactStorage from owned
input/snapshot cleanup outranks all pre-stage causes. Unknown wins mixed stage
causes. All error returns have zero seal. No private path/content is returned.

### Worker B — source helper + mock tests only

ONLY `internal/core/services/probe_binary_stage_receive_source.go` and `_test.go`.
No domain/port or service-A edits. Reuse probeBinaryStageNil for nil detection.
Expose exactly these package-private shared APIs:

```go
func newProbeBinaryStageReceiveSource(ctx context.Context, input io.ReadCloser, size int64) (*probeBinaryStageReceiveSource, error)
func (s *probeBinaryStageReceiveSource) Child() io.ReadCloser
func (s *probeBinaryStageReceiveSource) Complete() bool
func (s *probeBinaryStageReceiveSource) ConfirmEOF() error
func (s *probeBinaryStageReceiveSource) Finish() error
func probeBinaryStageReceiveCloseInput(input io.ReadCloser) error
```

Constructor owns input even on refusal. Validate typed nil/context/size in
(0,domain.MaxProbeBinarySize] before context methods. CloseInput is only for
service refusals before constructing a helper, closes once per call, skips nil/
typed-nil, sanitizes failure to bare ArtifactStorage. Normal helper owns one outer
Close via sync.Once, retaining its error; context.AfterFunc closes outer input
and signals callback completion. Finish is idempotent, closes outer and joins an
already-running callback (stop(false) is NOT a join); calling Finish twice must
not wait for a stopped-never-started callback. Return bare ArtifactStorage on any
outer close failure. Capture no private diagnostics.

Child is one stable owned reader of EXACTLY size bytes. Its EOF at limit is
synthetic; never consume byte size+1. Track remaining, terminal source failures
and concurrent lifecycle safely. Bound consecutive (0,nil) reads at 100 and
reject invalid Read counts before io.Copy can panic. Context checked before
reads. Read handles n>0 with error per io.Reader contract. Child.Close is
idempotent: after clean full consumption it does NOT close outer (needed for EOF
check); incomplete, failed or still-reading Close closes outer to unblock and
joins any active Read. New reads after Close are refused. No lock held while
calling underlying Read/Close. Complete means full consumption without a terminal
non-EOF source failure, no active read and a closed child; short source EOF is
incomplete. Concurrent child Reads fail closed; do not dispatch two input Reads.

ConfirmEOF requires Complete and live context. Read at most one extra byte;
require real outer io.EOF (bare or one unambiguous wrapping chain), never treat
(0,nil) or a joined EOF plus independent failure as completion. Bound 100 empty reads;
any byte is ArtifactSize, truncation/incomplete is ArtifactSize, non-EOF read
failure/invalid count/no-progress is ArtifactStorage. If the final payload read
already returned real EOF, still confirm on outer (normal Reader EOF is sticky).
No automatic arbitrary-size allocation or goroutine for reads. Before helper
return/cancel teardown, join every callback/read user it owns. No real I/O/tests
in worker test code; mock readers/closers only.

## Main ownership / acceptance

Main owns this doc, independent `internal/adapters/provisioning/local_binary_stage_receive_integration_test.go`,
all existing documentation and all executed gates. Existing preparation, writer,
inspection, snapshot, archive, SSH, journal/runner and schema Go bytes stay unchanged.
Workers must not run commands/tests/network, read credentials/home, spawn, Git or
edit other files. Requested model Grok4.7; serving identity independently unknown.

Effects: actual preparer -> new receive -> actual fd writer, both ELF fixtures as
data (never execute), exact graph/marker/modes, source closed before writer, private
preparation scratch cleaned, borrowed parent survives. Refuse bad pin/ELF/digest,
short/extra/empty/no-progress input, missing EOF with cancellation, early preparer
return/foreign/partial result, source Close failure or blocked callback; no stage
created. EEXIST never adopts; post-mkdir cancellation/unknown preserves partials.
Close failures after a valid stage revoke seal as Unknown, no deletion/retry.
Frozen source/inventory, full native race, repeats, assertion mutants and untouched
control, build/vet/format/Linux CGO-free cross-builds and existing Make harness.
No new MariaDB acceptance and no new Linux filesystem execution claim merely
from cross-build or prior writer acceptance. No new Docker/VM action: prior .work
remains deferred until shared VM handles are gone, not killed by this task.

Implementation, native repeats, mutations, builds, whole-source race/skip audit
and supporting reviews are complete at the bounded scope below. Local `ce6e106`
records the preceding exact 27-file accepted snapshot; no commit-only test rerun,
push or deployment. Its historical cleanup deferral, test skips and verification
scope are not rewritten here.

## Corrections and validation ledger

**Summary / mechanism.** Independent integration found the new helper accepting
`errors.Join(io.EOF, anotherFailure)` as successful outer EOF because it used
`errors.Is(err, io.EOF)`. Receive then closed input and really staged bytes despite
an independent input failure. The helper also forgot terminal source-context
errors and overlapping-read refusal when reporting Complete to a broken preparer
that ignored a Read error. These are pre-acceptance new-code defects, not failures
of the committed writer or an incident in a running installer.

**Reproduction / falsification.** `control-01` did not compile: four worker test
lambdas accidentally named a []byte argument `int`; the main integration callback
also needed the existing `func() error` signature. Correct only those test forms.
`control-02` then had 89 named passes and three named/package failure events in the
mixed-EOF case. `eof-repro-01` repeated it three times: ordinary amd64/arm64 fresh
stages passed (nine named passes), while mixed EOF returned nil error in every
repeat instead of ArtifactStorage. Same bytes, pins, parent and no cancellation
falsified metadata, architecture and timing explanations. Debugger was unavailable;
source tracing isolated ConfirmEOF's multi-cause errors.Is success.

`boundary-repro-01` independently failed every repeat for mixed EOF during payload,
source Canceled/DeadlineExceeded while caller remained live, and overlap followed
by full bytes. The latter two tests deliberately ignore the first Read error;
Complete must still be false. No assertion depends only on a method name.

**Fix.** Accept bare or bounded single-chain wrapped EOF, never multi-cause EOF;
keep any other terminal source error sticky regardless of the caller's context;
set failed on overlapping reads before refusing that read. An interim overly
strict direct-EOF equality failed two legitimate wrapped EOF/UnexpectedEOF tests
in `control-03` (282 passes, seven named/package failures). Preserve those tests
and support their single-chain status instead of deleting them or admitting joins.
Corrected `control-04` passed 288 named cases across three race repetitions.

**Why this was missed / test correction.** Ordinary readers emit bare EOF; initial
worker tests covered sanitized single wraps, not independent errors.Join causes
or a preparer ignoring an error after full-size bytes. The separate test-effect
review also found two instantaneous default-select assertions that did not observe
an actually held lifetime. Bound the event waits, hold Close/Read for a 50ms
no-return observation and register release cleanup. The active-read-join mutant
now triggers that intended assertion. This is test-strengthening, not a production
change; the first whole-source campaign was deliberately interrupted after its
build/vet/artifact/cross-build stages, never counted as acceptance. Exact process
group ownership/ancestry was verified before termination; shared VM untouched.

Two test files changed for that correction; production was identical. The complete
second campaign re-freezes and reruns all gates. Root interrupted/failed records
remain immutable; use `run-02/` via CURRENT, not root progress as ongoing success.

## Final frozen campaign

Source+inventory SHA256:
`754f027b12664ada45ceb106bf385fe81c3c06266a6d5318eb358b1c9d823e18`,
**1,518 source files**, baseline `ce6e106`. All preceding 1,512 entries remain
byte-identical; six new Go files only. New staging input/provenance APIs are not
inferred from a marker, command-line strings, DB timestamps or observation receipts.

| Executed gate | Result |
|---|---|
| Build / vet | Passed, 20.443 / 4.980 s |
| Five-package artifact race/count-3 | **2,466 named passes**, zero failures/skips, 63.538 s |
| Focused receive race/count-3 | **288 passes**, zero failures/skips |
| Intended assertion mutants | **12 caught**, then **96-pass untouched control** |
| CGO-free Linux probe/admin amd64 / arm64 builds | Passed, 14.339 / 16.091 s, not executed |
| Existing Make release/parser/shard harness | **24+4+27+17 tests**, plus release controls |
| Whole-source `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | **6,575 named passes**, zero failures, 1,925.225 s |

The full audit has **23 passing +13 no-test =36 application packages** and
**547 skips =542 missing `TEST_MARIADB_DSN` +five known optional/helper**;
zero unknown skips, malformed JSON lines or stderr bytes. Staging passes are
289 including prior inspection/writer plus 96 new receive cases. No new MariaDB
coverage; default SQLite fixtures do not close broad M7 production-engine gates.
`golangci-lint` and `govulncheck` were unavailable. Vet/format fallback is not lint
or vulnerability scanning. Unchanged frontend/Helm gates were not run.

The twelve mutants independently remove real EOF confirmation, input Finish
before Stage, unambiguous EOF classification, sticky source failure, sticky overlap,
payload progress bound, invalid-count guard, child byte bound, active-read join,
actual post-stage snapshot disposal, cleanup-to-Unknown classification and input
Close error propagation. Each caught the intended named assertion; compile errors,
panics, races, timeouts and zero selections are not catches.

Source review found no concrete blocker; test review's two blocked-observation
weaknesses were corrected and independently exercised. Reviews are read-only,
not gate execution. All requested subagents selected `grok-4.7`; actual serving
identity was not independently verified.

Only native Darwin/arm64 I/O is newly executed. No Linux runtime, real OpenSSH,
remote delivery, DB authority/fencing, host ownership, resource cleanup, installer,
authenticated enrollment or exact activation acceptance. Previous writer Linux
results do not prove this new composition. No new container/image/DB/remote host,
dependency/schema/framework or endpoint/startup change. No new commit of this
receive continuation, push or deployment. Broad M7/M6/release acceptance remains
open. Logs/manifests/failed and interrupted attempts live under
`tmp/m8-binary-stage-receive-verification/`; final source copies are retained.

