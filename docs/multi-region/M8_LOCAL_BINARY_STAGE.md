# M8 — fresh-only local binary staging writer

## Frozen scope

Continue after the uncommitted read-only binary staging inspection, without
changing any of its Go files. This slice has a real local filesystem effect:
materialize a release-named verified binary plus canonical marker under a borrowed
trusted directory descriptor. It is NOT an SSH uploader, DB-authorized executor,
installer, host fencing protocol, cleanup mechanism or activation path. No runtime,
HTTP, CLI or startup wiring; no network transport, credentials or actual /opt
writes. Production never resolves /opt. A test-only Lstat checks that its canonical
UUID path was not created; all write parents are disposable test directories.

SFTP v3 MKDIR returns no directory fd and later absolute-path OPEN re-resolves
parents. EXCL protects only the final file component. Therefore do not append
mutators to the verified inspection codec or pretend an LSTAT/OPEN sequence gives
inode identity. This separate local primitive is a building block for a future
fixed-command host helper; delivery and current authority remain separate work.

## Actual domain/port and public APIs

Read the actual new domain/port before callers:

- `internal/core/domain/probe_binary_stage_write.go`
- `internal/core/ports/probe_binary_stage_write.go`

```go
type ProbeBinaryStageSeal struct { Intent ProbeBinaryStageIntent }
type ProbeBinaryStageWriter interface {
    StageBinary(context.Context, domain.ProbeBinaryStageIntent, PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error)
}
func NewProbeBinaryStageWriteService(p ports.ProbeBinaryStageWriter) (*ProbeBinaryStageWriteService, error)
func (s *ProbeBinaryStageWriteService) Stage(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error)
func provisioning.NewLocalBinaryStageWriter(ctx context.Context, parent *os.File) (*LocalBinaryStageWriter, error)
```

The adapter implements `StageBinary`. Constructor borrows `parent`; never closes
it or the prepared binary. Each call duplicates the parent fd safely under
`SyscallConn.Control` with close-on-exec; verify directory type. Own and close/join
all duplicate/child/file descriptors and every stream returned by `binary.Open`,
even partial/error returns. `Open` is called at most once. No retained adapter fd,
no adapter Close API. Caller keeps the borrowed parent and source live; closing a
parent before a call fails closed rather than resolving a path or using a reused fd.

Domain receipt shape only repeats full intent; `DirectoryName()` is exactly the
single component `uptime-phoenix-probe-<operation_id>`, never `/opt` or another
absolute path. Identity host/owner/attempt are immutable labels, NOT current DB
or host authority. Neither seal nor name claims the supplied fd is /opt or on
that host. Never return `ProbeInstallResourceOutcome` or implement ApplyResource.

## Service (worker ownership)

ONLY NEW `internal/core/services/probe_binary_stage_write.go` and its `_test.go`.
Validate nil/typed-nil context/service/port/source, intent, source Pin exact match
before delegating. Getter returns immutable value; copy it at entry. No real I/O,
Open, owner Close, retry or goroutine in service. Call writer exactly once.
After delegation, any observed caller cancellation, malformed/foreign receipt,
error-bearing receipt or unknown/private error yields zero seal and bare
`ErrProbeBinaryStageWriteUnknown` (the call may have mutated). Do not return a
context sentinel after a potentially effectful call just to hide its effects.
If context remains live and port explicitly reports one of the four new errors,
sanitize wrapping via errors.Is to its bare sentinel. A context error returned
by a port while caller context remains live may be preserved as a pre-create
port refusal. Context canceled/deadline and input errors are allowed before
invocation. Never return private diagnostics. Zero result on all errors.

## Adapter (separate worker ownership)

ONLY NEW `internal/adapters/provisioning/local_binary_stage.go`,
`local_binary_stage_unix.go`, `local_binary_stage_other.go`,
`local_binary_stage_test.go`. Unix implementation is linux || darwin;
unsupported platforms refuse before mutation. Use existing x/sys/unix (already
a direct CGO-free dependency); no module changes. Reuse existing `imageSnapshotNil`
for adapter interface nil checks and `validProbeBinary` for copied ELF/build metadata.
Do not edit those existing helpers or artifact/snapshot/SSH implementations.

1. Validate context/intent/source and exact pin before I/O; duplicate/check parent.
   Open borrowed source once, own partial streams, close on every path; establish
   joined context revocation with sync.Once and a completion channel. Read Close
   must unblock Read. Pre-create refusal leaves parent completely unchanged.
2. Check cancellation immediately before one `Mkdirat(parentFD, name, 0700)`.
   No pre-check grants authority. EEXIST (directory/file/symlink) is fixed conflict,
   never opened, adopted, followed or repaired. Once mkdir is attempted, any other
   error/cancel is fixed Unknown, even ambiguous syscall failure. Never delete,
   unlink, rename, retry or resume partial output. A second call always conflicts.
3. Open the new directory via `Openat` with O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC and
   retain its fd. All children opened relative to it, with no-follow, nonblocking
   and close-on-exec flags. Create binary exclusively with mode 0600, stream
   size+1 bounded source input using a fixed buffer and progress checks. Never
   store the excess sentinel byte. Require exact size, client SHA256 and existing
   static Linux ELF/Go probe checks. No extraction or execution.
4. Sync, chmod binary 0555, close writer. Independently reopen read-only relative
   to the retained child fd, require same device/inode, regular type, exact mode,
   size and single link; re-hash bytes to EOF and reuse metadata validation.
   Close input and join its callback before publishing marker; source Close
   error or cancellation means Unknown and no successful seal.
5. Exclusively create `.phoenix-stage-v1` with exact existing intent.Marker()
   bytes, Sync/chmod 0444/close, then independently reopen and verify identity,
   regular type, exact size/mode/single link/hash. Short writes never count as
   complete. No raw content/paths escape in errors.
6. Check directory still bound to the same inode via parent fd, and ensure only
   the two expected entries with the recorded file identities. This is a bounded
   final check, not permanent/atomic closed-graph proof. Sync child and parent,
   join/close every owned resource and check cancellation before returning seal.
   Any post-attempt failure (including readback/Close/Sync/unknown entry/replacement)
   is bare Unknown; leave all effects untouched. No partial result on error.

Trusted-parent model: parent is an existing operator-held, trusted local directory,
not a user pathname. It and the mkdir/open acquisition interval must not be
modified by hostile/cooperating external mutators. The retained fd prevents later
pathname re-resolution from redirecting child writes; it does not stop same-UID
or privileged actors replacing contents. A parent pathname rename is harmless
and does NOT prove its new absolute name. Replacing/removing the child name after
fd acquisition must revoke success, while writes must never follow replacement
paths. No UID ownership, cross-process lease, durable host epoch, cleanup token,
remote termination or current DB authority is inferred. Files may change after
return. Blocking kernel filesystem calls are not given invented hard deadlines.

Unexported per-instance fault hooks are permitted for tests (e.g. after child fd
acquisition, before readback/marker); no globals or public arbitrary-path API.

## Main ownership / acceptance

Main owns domain tests, independent real service->prepared artifact->filesystem
integration tests, documentation and execution. Parallel workers run no commands,
network/tests, credentials/home reads, agents, Git or other edits. Requested model
is Grok4.7; serving identity is not independently verified by requesting a model.

Test actual bytes and effects: fresh successful exact graph/modes/identity,
both architectures using actual static probe-shaped fixture; borrowed source and
parent survive; second/concurrent calls conflict without overwrite or adoption;
pre-existing dir/file/symlink including outside canary unchanged; late source error,
size/digest/ELF mismatch, short writes, source-close cancellation, blocked-source
join, corruption before readback, marker/close/fsync faults, unknown entries,
parent pathname rename and child replacement. Pre-create invalid/cancel/input
failure leaves no child. Post-create failure leaves honest partial output and
zero seal/Unknown; no cleanup/replay. No executable/canary is run.

Freeze source+inventory and prior inspection bytes; build/vet/gofmt, race repeats,
full race default-skip audit, Linux CGO-free builds, assertion mutants then untouched
control, existing Make/parser/shard checks. A Linux cross-build is not Unix
filesystem runtime acceptance: report native engine/OS explicitly; separate actual
Linux runtime acceptance may use an isolated local test container with no public
pulls, network disabled and task-owned resources only; no actual probe execution.
The executed check below used a task-owned empty scratch image because no images
were cached on the selected local daemon.

Implementation and frozen verification are complete at the bounded scope below. Preserve the prior 16 uncommitted files and
historical evidence, including `tmp/m8-binary-stage-verification/CURRENT.json`.
No Git commit/push, default opt-in, DB/schema/fence change or live host action is
authorized by this continuation.

## Engineering corrections and reproduction ledger

**Summary.** Independent integration found three pre-acceptance defects in the new
writer, not an incident in shipped provisioning: an unconfirmed EOF could produce
a seal, repeated zero-progress reads were unbounded, and cancellation at the
marker boundary could still publish that marker. The focused fixes restore
explicit source completion and a last pre-marker stop check.

**Symptom / root cause.** `writeSource` accepted one `(0, nil)` extra read as if it
were EOF. A source paused exactly after the pinned bytes then offered an excess
byte, but the call sealed without reading it. Its main copy loop also lacked an
empty-read counter; a deterministic reader returning 100 empty reads then a
failure proved more than the allowed 100 reads were dispatched. Finally, source
closure was checked before `beforeMarker`, but caller cancellation during that
boundary was only noticed after marker creation. Return was already Unknown,
but the additional marker effect was observable and should not have been started.

**Fix.** Require actual EOF within at most 100 empty extra reads; bound consecutive
empty reads in the main loop and reject invalid read lengths; check caller context
again between the pre-marker hook and exclusive marker creation. No readback,
metadata, exclusive-create or uncertainty check was weakened. A worker test also
referenced a nonexistent `stagePrepared.stream` field; remove that redundant test
initializer, retaining the actual Open callback. That compile error is not a
runtime defect or an assertion-mutant catch.

**How found / falsification.** Real service→actual prepared fixture→fd writer
integration established ordinary amd64/arm64 success while all three boundary
probes failed in every repetition. Thus fd acquisition, ELF architecture and
ordinary hash checks did not explain the failures. Debugger was unavailable;
source tracing isolated the three guards. `io-repro-01` retained 24 named passes
and 16 named/package failure events across three repeats, with the exact assertions
`unconfirmed EOF accepted`, `zero progress was not bounded 101`, and
`marker published after known cancellation or source failure`.

**Why it slipped through.** New worker happy-path/file readers return immediate
EOF and did not pause at the exact marker cancellation boundary. Main's independent
integration supplied those missing lifecycle states before accepting the slice.
`control-01` retains 55 passes and an adapter build failure; `control-02` retains
76 passes and six named/package failure events. Neither is accepted evidence.

**Validation.** Corrected `control-03` passed 243 named cases; expanded `control-04`
passed **246** across three race repetitions with zero failures/skips. The extra
marker-corruption case and the binary padding-byte corruption isolate readback:
the latter restores mode 0555 and requires `validProbeBinary` to remain true, so
mode/ELF rejection cannot mask a skipped hash. Actual Linux effect repeats also
pass these cases. This branch remains uncommitted; no historical checkpoint was
rewritten to pretend it covered the new writer.

## Frozen verification and limits

Source+inventory SHA256:
`ef07bb82bd765bfdb3e9c3ef054d72544abf63a671d4135e523ca573a61c6b50`,
**1,512 files**, baseline `b9cb6ed`. Every prior source entry from the 1,502-file
inspection freeze remains byte-identical; only ten new Go files were added.
The clean mirror copies all tracked/nonignored project files, uses the exact
backend-CI embed placeholder and a private GOCACHE. No application package,
scenario, fence or assertion is dropped; ignored dependency/tool trees are outside
that CI surface. Documentation/configuration get separate final manifests.

Executed against the frozen source:

| Gate | Result |
|---|---|
| `go build ./...` | Passed, 17.889 s |
| `go vet ./...` | Passed, 5.176 s |
| Five-package artifact race/count-3 repeat, including `LocalBinaryStage` | **2,178 named passes**, 567 staging (old inspection + new writer), zero failures/skips, 50.394 s |
| New writer focused race/count-3 repeat | **246 passes**, zero failures/skips |
| CGO-free Linux probe/admin amd64 / arm64 cross-builds | Passed, 13.162 / 13.487 s; resulting probes not executed |
| Intended assertion mutants | **14 caught**, then **82-pass untouched control**, zero control failures/skips |
| Existing Make release/parser/shard checks | **24 correspondence +4 release +27 dual-engine-parser +17 shard tests**, plus existing release controls |
| Full-source `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | **6,479 named passes**, zero failures, 1,923.539 s |

Full audit accounts for **36 application packages: 23 passing and 13 no-test**;
193 staging passes include the previous 111 inspection cases plus 82 new writer
cases. **547 skips =542 missing `TEST_MARIADB_DSN` +five known optional/helper**;
zero unknown skips, malformed event lines or stderr bytes. No new MariaDB
acceptance is claimed; broad operative M7 MariaDB and M6/release/deployment gates
remain separate/open. `golangci-lint` and `govulncheck` were unavailable; vet and
format checks are not completed lint/vulnerability scans. Frontend/Helm gates
were not run because those surfaces are unchanged.

Mutants exercise confirmed EOF, bounded progress, last marker cancellation check,
binary hash readback, exact marker readback, source-close-before-marker,
unknown-entry graph refusal, child-name binding, borrowed parent lifetime,
parent fsync and binary-close acknowledgement, post-attempt uncertainty, exact
service receipt and service cancellation classification. Compile failures,
process panics, races, timeouts and zero selections are not catches.

### Actual Linux filesystem execution

A credential-free config and the recorded local Unix Docker endpoint were used.
No cached image was present, so an empty all-zero tar was imported as an exclusively
task-labelled scratch test image. It contains no probe binary or application rootfs.
A statically cross-compiled **Go test binary**, not a probe, ran on **Linux/arm64**
under UID/GID 10001, read-only rootfs and read-only fixture mounts, private writable
tmpfs, all capabilities dropped, no-new-privileges, resource limits and **no network**.
No pull, image publication, daemon mount, systemd process, DB or SSH action.

The actual prepared-binary/service/writer effect tests passed **45 named cases**,
with **12 required leaves each executed three times**, zero failures/skips. Both
amd64/arm64 ELF fixtures are read as data; the execution canary is absent. This
Linux test binary is CGO-free **without race instrumentation**; Darwin race gates
are separate. Exact owned labels/IDs were checked before removing both the stopped
test container and scratch image; recorded baseline containers were preserved.
Logs, command metadata, fixture/test hashes and coverage are retained under
`linux-runtime-01/`. This closes only the new local writer's exercised Linux/arm64
filesystem effects—not general Linux/OpenSSH deployment, remote delivery, the old
image snapshot/archive filesystem matrix or Linux/amd64 runtime coverage.

Two separate Grok-requested read-only source/test reviews reported no concrete
scoped blocker; reviews executed no gates and serving identity was not independently
verified. No new MariaDB coverage, database/lease authority, host ownership/fencing,
cleanup authority, authenticated enrollment/config activation or installed success.
Evidence lives at `tmp/m8-local-binary-stage-verification/`; prior inspection evidence
is unchanged. No new commit/push/deploy, live remote host or resulting probe
execution. Only identified task scratch may be removed after joined users and
identity checks; failed attempts, frozen source and acceptance manifests remain.
Scratch cleanup was **deferred**, not forced: lsof still reports the shared macOS
Virtualization VM holding handles to the Linux test binary/fixture subtree after
the exact task container and image were removed. The entire owned `.work` remains;
no shared VM was stopped or cache pruned. See `completion-audit.json` and
`cleanup-deferred-open-handles.json`; this is a cleanup limit, not a failed test
silently promoted to acceptance.
