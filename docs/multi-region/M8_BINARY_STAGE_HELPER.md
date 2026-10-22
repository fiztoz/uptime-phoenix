# M8 — one-shot inherited-fd local binary staging helper

## Frozen scope

Continue after local commit `eb0dd00` of exact-stream receive. Add one concrete
config-free `phoenix-probe-admin stage-binary` verb (zero flags/arguments) that
stages verified bytes beneath an explicitly inherited directory descriptor 3.
No destination-path input or identity-derived /opt target open; private preparation
uses the OS temporary directory (which the trusted launcher can select via TMPDIR).
No SSH/remote-helper deployment, DB/journal call,
lease/fence/ownership/cleanup authority, installer, enrollment or activation.
The host/owner/attempt in metadata remain labels. Never implement the executor or
map a seal to artifact_directory/applied: an arbitrary parent fd is not /opt.
No existing core/preparation/receive/writer/inspection/image/SSH/schema changes.

This is executable local progress, not another service facade. Its success means
only a fresh local staged graph under the inherited directory. Context budgets
revoke stdin, not arbitrary blocking kernel calls; do not claim a hard process
termination bound or remote cancellation. No default startup opt-in.

## Wire contract (no domain JSON)

One stdin stream: 8-byte ASCII magic `PHXSTG1\n`, four-byte big-endian unsigned
JSON byte length N (1..4096), exactly N bytes strict UTF-8 JSON, then the raw
pinned binary, then REAL stream EOF. No bufio prefetch, payload LimitReader,
second stdin reader, trailer, compression, path, private key or command field.
Header reads consume exactly prefix+N; pass the same post-header closer to
Receive so it independently verifies payload EOF. Bound header no-progress to
100 consecutive empty reads and reject invalid reader counts without panic.

JSON is one flat object with EXACTLY these 14 required canonical names:

- operation_id, owner_id, attempt_id
- ssh_address, ssh_user, host_key_fingerprint, client_key_fingerprint
- requested_by (int64), layout_version (int), mode
- version, architecture, sha256, size (int64)

All other fields, duplicate decoded names (including escaped duplicates), escaped
key spellings, case aliases, malformed/non-object/trailing JSON, invalid UTF-8, nulls/wrong types and
missing keys are refused. No arbitrary nested payload. Build explicit DTO with
json snake_case tags and then assign domain values; do not marshal/unmarshal a
domain struct. Require full intent.Valid(), systemd/layout1 and zero unused image.
Numeric nonintegers/overflow/negative/zero fail existing validation. Canonical
encoder must bind every field, produce deterministic prefix+JSON, never payload.

### Worker A — pure header codec only

ONLY NEW `internal/adapters/provisioning/binary_stage_header.go` and `_test.go`.
No commands/tests/network/credentials/home/spawn/git or other edits. Read exact
new doc, actual domain/probe_binary_stage.go, probe_install_journal.go and
probe_binary_artifact.go before callers. No I/O in codec/tests; byte fixtures only.

```go
const ProbeBinaryStagePrefixSize = 12
const MaxProbeBinaryStageHeaderSize = 4096
func ProbeBinaryStageHeaderLength(prefix []byte) (int, error)
func DecodeProbeBinaryStageHeader(header []byte) (domain.ProbeBinaryStageIntent, error)
func EncodeProbeBinaryStageHeader(intent domain.ProbeBinaryStageIntent) ([]byte, error)
```

Length requires exactly 12 prefix bytes, exact magic and bounded N. Decode
requires header length exactly 12+N, no body/trailing bytes. Every codec refusal
is zero intent/nil bytes and bare ErrProbeBinaryStageWriteInput. No new domain
errors or dependencies. Exact key-set/type/duplicate validation is needed because
encoding/json.DisallowUnknownFields alone accepts case aliases and duplicate keys.
Tests successful round trip plus independent decoded field/pin/marker comparison,
not only Encode->Decode cancellation; both architectures, max valid identity,
all typed value/key/prefix/UTF8/size/mode/field corruption cases, no panic/excess
allocation for hostile prefixes. No claimed test execution by worker.

### Worker B — bootstrap runner and mock boundary tests only

ONLY NEW `internal/bootstrap/probe_binary_stage_helper.go` and `_test.go`.
Read contract + actual Receive, domain/ports and existing bootstrap/probe_binary_artifact.go
before callers. Main owns inherited-fd platform helper files and actual I/O tests.
No commands/tests/network/credentials/home/spawn/git or other edits.

```go
func RunProbeBinaryStageHelper(ctx context.Context, args []string, parent *os.File, input io.ReadCloser, out, stderr io.Writer) int
func RunInheritedProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int
// Main implements these same-package helpers in platform files:
func inheritedProbeBinaryStageParent(ctx context.Context) (*os.File, error)
```

Borrow parent in Run; own non-typed-nil input on EVERY return; typed nil never
method-dispatched. Validate context/args/output/sink/input before methods or parent
I/O. Zero args only: malformed/--help/--path all fixed usage exit2, stdout empty,
close input; do not echo args. Nil/invalid context/input/parent -> fixed failure.
Create a context.WithTimeout(ctx,2*time.Minute), then a once-owned guard around
input: Read checks caller context and delegates WITHOUT buffering/limiting body;
private closeRaw invoked by context.AfterFunc, public Close closes once and
joins its callback, idempotent even if stop(true) happened earlier. No lock held
across raw Read/Close. Final guard.Close is required even on header refusals;
input-close failure before any Receive must not be hidden. Receive will itself
Close this guard before Stage, and runner's later Close is idempotent, never a
second raw Close. Header read loop checks context, 100-empty limit and counts.

Construct unchanged local writer under parent (constructor no mutation), read
exact bounded header, decode, construct real existing preparer with empty baseDir
(OS temporary directory, not header path), existing receive service and one
Receive(ctx,intent,guard). No direct Open/Close of snapshots in runner; Receive
owns those. Never resolve intent.Directory(). Refused header/body must leave
parent unchanged. Existing name conflicts, never adopts or cleans it.

Return0 only for exact valid seal, live context, finished input, and acknowledged
complete output. Explicit response View only, one JSON line of <=2048 bytes:
state=local_binary_staged, directory_name (relative), marker_sha256,
pinned_version, version_source=operator_pin, os=linux, architecture, sha256, size.
Exactly9 fields. The os=linux field describes the pinned ELF target, not the helper
host OS. No operation/host paths, password/key/internal capability data,
applied/installed booleans or raw domain. Marshal into bounded bytes before one
Write and check short counts as well as error. After a Receive attempt, unknown/
private/nonexact/cancel/close/output failure is nonzero, zero trusted receipt,
bare ErrProbeBinaryStageWriteUnknown on stderr where effects may exist. Preserve
bare explicit received stage/artifact/context refusal only when no error-bearing
seal and caller live. No returned private diagnostics. Error stderr is ONE fixed
allowlisted line, never echo input/frame/args/path/host keys. Usage exit2; other
failures exit1. Short/failed output can be partial bytes; consumers MUST require
complete single JSON newline AND exit0. A receipt without zero exit is not ack.

RunInherited duplicates/checks fd3 through main-owned platform helper, not a path.
If duplicate fails, close input once and report unavailable without hub config.
It owns only that duplicate, never closes fd3 or a caller's parent. After acquiring
that parent, main's platform input adapter consumes file stdin. Regular files
retain their ordinary filesystem semantics. Pipe stdin is duplicated with
CLOEXEC, temporarily made O_NONBLOCK and wrapped in a fresh os.File so Go's
poller can revoke blocked reads. Close first closes/joins the pollable duplicate,
then restores the original status flags and closes the owned original stdin.
Non-file inputs retain their caller-supplied Close-unblocks-Read contract;
directory/character/socket file stdin is refused and closed, not probed.
This changes only the owned input, never fd3, and invents no bound for kernel
filesystem work. The adapter is in main-owned platform files, not core Receive.
Call borrowed Run with bounded local response and diagnostic buffers (bytes.Buffer is safe because Run caps
its output), then CLOSE the parent duplicate BEFORE flushing any successful JSON
to actual out. Failed inner calls discard their entire buffered receipt and
reconstruct exactly one allowlisted diagnostic; no partial inner buffer is forwarded.
Parent-close, context, short/output failure after possible effects is Unknown,
no retry/delete. Receive's bare pre-stage ArtifactStorage (including a sticky
source Close failure) remains ArtifactStorage for a live caller, not invented
post-stage uncertainty. Make source ownership unambiguous on all branches;
no double close after Run owns input. Nonempty args rejected BEFORE fd3 use.

Mock tests (no actual files/network) cover args/nil/typed-nil/context/input ownership,
fixed secrecy, no-progress/invalid count header boundaries where reachable, short
output writer behavior via package-private pure writer helper if needed. Main
independently executes real parent/input/success/cancel/close/output cases. Do not
modify developing Worker A files; use the exact shared codec signatures.

## Main ownership and actual effects

Main alone edits cmd/phoenix-probe-admin/main.go to early-dispatch stage-binary
before LoadConfig and implements `internal/bootstrap/probe_binary_stage_parent_unix.go`
(linux||darwin; fd3 F_DUPFD_CLOEXEC, os.NewFile, Stat directory; close duplicates on
all errors; ctx checks) plus `_other.go` refusal. No os.NewFile(3) finalizer owning
the caller's fd. Main owns CLI ExtraFiles subprocess and bootstrap real-I/O tests,
all docs and all gates. Requested subagent model grok-4.7, serving identity unknown.

Acceptance: actual command production entry point with disposable fd3, invalid hub
environment and no DB files, strict same-stream header+body+EOF; both actual static
Linux ELF fixtures as data, never execute probe/canary; exact graph/marker/modes,
relative bounded9-field response, source/parent ownership and clean prep temporary
storage. Missing/wrong fd3, args, bad/duplicate/case/null/size/mode header, short/
extra body, bad digest/ELF, EOF withholding/cancel must never report success.
Bad pre-body input makes no stage; replay conflicts with bytes intact. Process
signal tests only target identified task child. Bootstrap output/close failures
after real stage are Unknown with graph preserved; never pretend not_started.

Freeze all source+inventory and prior bytes (only main.go changes among old Go),
focused native race repeats, full race/default skips, build/vet/format, Linux CGO0
cross-builds, intended assertion mutants + untouched control, existing Make gates.
Only native Darwin process/fd execution is newly claimed unless separately run.
No new Docker/VM/DB/live host action; earlier writer .work remains deferred. No
shared cache prune or deleting unknown paths. No installer/distributed-authority
acceptance inferred from a local process receipt.

Implementation and executed verification are complete at the bounded run-03 scope
below; owned scratch cleanup is a separate identity/no-user gate. Receive checkpoint
eb0dd00 is local only, not pushed; prior evidence/cleanup limits stay immutable.

## Reproductions retained so far

- `control-01`: two positive codec failures. Decoder.Token consumes a separator
  comma but its prior InputOffset does not; canonical key validation accidentally
  included that comma. Prefix sizes and independent valid intent fixtures already
  passed. One-variable private overlay removes only that separator: both original
  positives passed three repetitions; production correction preserves strict keys.
- `control-02`: known pre-stage input-Close errors were unnecessarily changed to
  Unknown by helper's repeated Close check; Receive already proved no Stage.
  Same campaign's actual process EOF/SIGTERM check stalled until child deadline
  kill, then third repeat hit package timeout. Not acceptance; retained logs.
  No task helper processes survived the post-run process observation.
- `stdin-repro-01`: tighter per-child deadline reproduced the signal/join failure
  three times. Actual inherited blocking os.Stdin is not the pollable io.Pipe
  used by service tests. Go's NewFile/read/Close implementation explains the
  difference. New owned pipe wrapper and flag restoration fix that entry boundary;
  no core source contract was weakened and no hard remote-stop claim was added.
- `control-03`: 321 native race passes; actual built admin executable with fd3,
  both static Linux ELF canary fixtures as data, and signal test all passed.
- `control-04`: 342 native race passes after independent flag-restoration,
  inherited failed-buffer/close/short-output and callback-tail coverage.

Worker orchestration timed out at 900s with both file pairs present, one completed
handoff and one canceled handoff. No worker test execution is acceptance. Main
owns the independently executed controls and all later frozen gates. A write guard
refused an unnecessary credential-shaped dummy field; the actual unknown-field
fixture uses a plain extra_field instead. No credential was used or disclosed.

## Test-effect corrections and final-source binding

The independent production review found no concrete blocker at its read-only
scope. Test review identified that a deadline-killed child could satisfy a loose
nonzero refusal assertion, and that Linux canary absence on Darwin is not execution
proof. Refusals now require normal exact exit1 plus their sentinel (usage exit2).
A foreign existing marker is left byte-/inode-/mtime-identical on conflict.
Canary inference is conditional on matching Linux; no probe fixture is executed
by a test command, and no Darwin canary proves that execution was never attempted.
Close-before-flush and fd3 non-closure also use source tracing, not a fabricated
external observation. Callback-tail and parent-close-failure models are explicit;
actual blocked callbacks, reads, stdin flag restoration and signal join are separate.

Two mutation survivors exposed independent fixture masking, not production defects:

1. `oversize-count-repro-01` removed the oversized-header-read guard yet passed
   three times. A concurrent private source error forced the expected refusal.
   The invalid-count fixture now returns nil error; the original mutant fails the
   intended count assertion three times in `oversize-count-repro-02`.
2. `run-02` caught15of16 mutants, with114 untouched control passes. Its mixed-EOF
   header fixture reintroduced the same error on body reads, so the preparer hid
   a missing header completion guard. The fixture now faults ONLY the first header
   read; `mixed-header-repro-02` catches that original mutant three times.

Whole-source campaigns at the root and `run-02/` were deliberately stopped and
their identified task process groups joined before changing these two tests.
Their incomplete suites are retained, never promoted to acceptance. No shared
VM or unrelated process was stopped. `control-07` passed342 native race tests.
All Go gates rerun on final source
`99622aa14e448b61b174f5b2f1fd7bb697f74ba082cf5d953299acecc36f3fdb`,
1,526 source files. Compared with the committed receive checkpoint, 1,517 prior
source entries remain byte-identical; only cmd/phoenix-probe-admin/main.go changes,
with8newGo files. No core/port/adapter-writer/preparer/SSH/schema implementation
changes. The final gate evidence and separate owned-cleanup records are below.

## Accepted native verification

Final run-03 uses installed Go1.26.9, sanitized offline child environments, all
tracked/nonignored project inputs in an exact CI-surface mirror, the backend CI
embed placeholder and task-private build cache. No app assertion/package/fence is
omitted. No inherited DSN/credentials, new dependency, public pull or live host.

Executed commands (source mirror; exact argv/logs are retained):

```sh
go build ./...
go vet ./...
go test -race -count=3 -json -timeout 600s \
  ./internal/core/domain ./internal/core/services ./internal/adapters/provisioning \
  ./internal/bootstrap ./cmd/phoenix-probe-admin \
  -run 'Probe(Binary|Image)|ProbeAdmin(Binary|Image)|ProbeAdminStageBinary|LocalBinaryStage'
go test -race -count=1 -json -timeout 3600s -p 4 ./...
go test -race -count=3 -json -timeout 300s \
  ./internal/adapters/provisioning ./internal/bootstrap ./cmd/phoenix-probe-admin \
  -run 'BinaryStageHeader|BinaryStageHelper|StageBinary'
make release-image-gate m6-backend-harness-gate
```

- Build21.558s; vet6.009s; artifact repeat **2,808namedpasses**, no skips/failures,
  77.279s; focused repeat **342passes**, no skips/failures, 11.440s.
- CGO0 Linux amd64/arm64 builds of probe and admin passed13.858/13.962s.
  These artifacts were not executed; cross-builds are not Linux runtime proof.
- Whole-source native race: **6,689namedpasses**, **403staging**, zero failures,
  **1,914.200s**. All6,575 prior named passes remain;114new named passes.
  Exactly23passing+13no-test=36applicationpackages, same package inventory digest.
- **547skips**:542missing TEST_MARIADB_DSN plus5known optional/helper cases;
  no unknown skip, malformed JSON or stderr bytes. No new MariaDB acceptance.
- All **16intendedassertionmutants** caught, then **114untouchedcontrolpasses**.
  Guards: duplicate/escaped key, header cap/count/progress/unambiguous EOF,
  active-read/callback-tail join, pollable stdin/flag restoration, complete output,
  failed-buffer discard, parent-close ack, cancel-after-output, pre-stage Close
  storage classification and actual early CLI dispatch. No compile, panic, race,
  timeout, signal-kill or zero-selection substitute is accepted as a catch.
- Existing release/parser/shard harness counts **24+4+27+17**, with controls,
  passed. golangci-lint and govulncheck unavailable; vet/format are not those scans.
  Unchanged frontend/Helm gates not run. No new core import; a whole-core scan
  rejects framework/driver/adapter imports. An initially overbroad metadata audit
  also rejected the pre-existing pure google/uuid import in probe_admin_service.go;
  that file is byte-identical to baseline, explicitly recorded and not refactored
  by this slice. The failed audit observation is retained, not a Go gate failure.

Actual helper process/fd/filesystem execution is native Darwin/arm64. Its built
admin subprocess is CGO0/non-race; parent integration and library tests run with
race detection. Both Linux canary ELF fixtures are data; Darwin canary absence is
not attempted-execution proof. No new Linux/OpenSSH/remote runtime, imported image,
current authority/fencing, installer, enrollment or exact activation acceptance.
Broad M7/M6/release gates remain open.

Evidence: `tmp/m8-binary-stage-helper-verification/CURRENT.json` points to run-03.
Source/docs/config manifests, coverage audit, review ledger, exact frozen18files,
mutants, logs and both interrupted campaigns are retained. `cleanup-result.json`
and post-cleanup `final-audit.json` are authoritative for removal of only this
helper's three identity-verified .work trees; passing tests alone never prove
cleanup. The prior local-writer .work is excluded and remains deferred by its
historical shared-VM handle finding; no recheck/VM stop/cache prune here.

The new helper continuation is18uncommittedpaths (8newGo, main.go and9docs),
index empty after local receive commit eb0dd00. No second commit, push, merge,
deployment, Docker/image/DB resource or actual remote-host action. Next specify
a trusted fixed launcher/current-host-authority boundary: ordinary SSH exec does
not create the fd3 association or authorize artifact_directory/applied.

## Subsequent local commit and fixed-parent continuation

The above completion snapshot was subsequently locally committed as
`718e3b826a4333a38a7de58d401816f969ae84a7`, parent `eb0dd00`, message
`feat(core): add one-shot M8 binary staging helper`. All18committed files were
byte-verified against run-03's accepted frozen snapshot; no commit-only test
rerun, hook bypass, push or deployment. The historical completion/cleanup records
above are not rewritten by that commit. Evidence:
`tmp/m8-binary-stage-helper-verification/run-03/{precommit,committed}.json`.

The separate [fixed Linux host-local launcher](M8_BINARY_STAGE_HOST_LAUNCHER.md)
now selects a protected literal `/opt` parent for `stage-binary-host`, without fd3
or a caller path, then reuses this unchanged helper. Its separately approved
isolated Linux tests are not retroactive runtime acceptance for this historical
Darwin campaign. Local root/namespace selection grants no current DB authority,
physical/SSH host identity, fence, applied-resource or installed-success claim.
