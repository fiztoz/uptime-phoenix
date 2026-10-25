# M8 — fixed Linux host-local binary staging launcher

## Frozen bounded scope

The inherited-fd helper is locally committed as `718e3b8` (parent `eb0dd00`),
exact18verified files; no commit-only test rerun or push. This continuation adds
one concrete way to select the host-local namespace's fixed staging parent.
`phoenix-probe-admin stage-binary-host` is an exact early verb, zero arguments,
Linux effective uid0 only. No shell, exec, SSH, free destination path, hub config,
DB/journal call, executor, service unit, Docker runtime install, enrollment or
activation in this command. Root privilege does NOT grant current DB authority,
fence another privileged/stale actor, or authorize artifact_directory/applied.

The user explicitly approved task-owned isolated local Docker image/container
verification with no network/pulls, read-only fixture mounts, dropped capabilities,
bounded resources, private tmpfs /opt, no host /opt access, no probe execution,
no shared-VM stop and no cache prune. A subsequent explicit approval permits two
root-inode fixture cases: writable private container layer, CAP_CHOWN for setup
of the owner case only, then clear capabilities and reexec the test before any
helper invocation. Their fixtures live on a read-only task-owned volume; runtime
asserts zero effective/permitted/inheritable/ambient capabilities and sticky
no-new-privileges. Approval is for verification, not deployment.
Only that isolated Linux namespace may exercise the positive literal /opt path.
Fixtures are copied into the task image's read-only root filesystem (or read-only
task volume for the two root-inode setups), never host bind mounts, avoiding the
earlier shared-VM bind-handle retention seam.
Native Darwin returns unavailable without opening /opt. Existing inherited-fd
helper, header, Receive, preparer, writer, domain/ports/SSH/schema stay unchanged.

## Parent selection (main ownership)

NEW internal/bootstrap/probe_binary_stage_host_parent_linux.go and _other.go:

```go
func openHostProbeBinaryStageParent(ctx context.Context) (*os.File, error)
```

Linux validates nil/typed-nil/live context, then geteuid()==0 BEFORE any path open.
Open literal `/` with O_RDONLY|O_DIRECTORY|O_NOFOLLOW|O_CLOEXEC. Fstat must report a
live directory, uid0 and mode&022==0. Openat only literal `opt` under that retained
root with the same flags; fstat applies the same checks. No environment, metadata,
argument, cwd or symlink-selected target. No mkdir/chmod/chown on root or parent.
Close root before returning an owned opt *os.File with an opaque constant name.
Close every owned partial descriptor on every error; cleanup close failure is
bare ErrProbeArtifactStorage. Other pre-stage platform/trust/open failures are
bare ErrProbeBinaryStageWriteUnavailable; invalid ctx is Input and live context
cancellation sentinels are preserved only after cleanup. Post-I/O context check.
Unsupported platforms return Unavailable without any target open. The shared
helperNil and fixed error helpers may be reused unchanged.

This selects a retained directory at acquisition in the current mount namespace,
not a physical/SSH host identity or perpetual /opt binding. Trusted namespace and
absence of hostile concurrent root/mount replacement are prerequisites. Its mode
and uid are local attributes, not operation ownership or a host fence. Private
preparation still uses the trusted launcher's OS temporary directory; stdin
metadata cannot select that directory. Existing operation-derived fresh child,
no overwrite/adoption/deletion, independent readback and Unknown semantics remain.

## Worker A — bootstrap wrapper and mock-only tests

ONLY NEW internal/bootstrap/probe_binary_stage_host.go and
internal/bootstrap/probe_binary_stage_host_test.go. Read this full contract and
actual probe_binary_stage_helper.go + parent_unix.go before callers. No other
edits, commands/tests/network/credentials/home/spawn/git. Exact public API:

```go
func RunHostProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int
```

Use private runHostProbeBinaryStageHelper with an injected acquisition function
of type func(context.Context)(*os.File,error) solely for mock boundary tests.
Validate args/context/typed-nil input/out/stderr before acquiring a parent. Own
non-typed-nil input on EVERY return, close once; no double close after unchanged
Run takes ownership. Nonempty args -> fixed usage2 before acquisition (close
failure -> ArtifactStorage exit1); never echo args. Usage text exactly:

Usage: phoenix-probe-admin stage-binary-host
No flags, paths, or arguments. Linux uid 0 only; fixed root-owned /opt parent.
This is NOT installation, current DB authority, fencing, enrollment, or cleanup.

Acquisition owns its partial descriptor on errors, but wrapper must defensively
close any nonnil error-bearing parent (close failure -> ArtifactStorage). Nil
success/unknown error -> Unavailable, sanitized context/storage allowed for live
caller according to existing helpers. Acquisition refusal never reads input.
After acquisition, reuse inheritedProbeBinaryStageInput (consumes raw even on
error), then RunProbeBinaryStageHelper into local bounded response/diagnostic
buffers. Own parent, close it exactly once BEFORE stdout; call unchanged
finishInheritedProbeBinaryStageHelper to discard failed buffers and enforce
Unknown on parent-close/cancel/short/output uncertainty. Do not call borrowed Run
straight into public stdout. Existing wire14keys and receipt9fields unchanged.
No new service/port/domain/error or input wrapper; no path/UID policy in Worker A.
Main supplies platform function above and ALL real filesystem/process tests.

Mock-only tests cover args/secrecy/typed nil/cancel-before-acquire, input-close
storage refusal, acquirer no-read/exact-call/nil/error sanitization. No fake
successful *os.File or invented successful filesystem proof. No executed claims.

## Actual Linux acceptance (main ownership)

NEW internal/bootstrap/probe_binary_stage_host_linux_integration_test.go; buildtag
linux. TestProbeBinaryStageHostSandbox is explicitly opt-in; absent
PHOENIX_M8_HOST_SANDBOX=1 skips in ordinary Linux go test, but the dedicated gate
must refuse missing configuration/skipped/zero/parent-only coverage. Test reads
only fixed /fixtures prebuilt admin and two static Linux probe ELF fixtures as
data; no Go toolchain needed inside scratch. It runs the ACTUAL admin executable
with invalid hub env and no inherited fd3. No probe artifact is executed. No
canary-absence inference when architecture/noexec mounts would hide an attempt.

Positive namespace: uid0, protected root, safe uid0 mode0755 tmpfs /opt. Required
leaf names under TestProbeBinaryStageHostSandbox/:

- ParentFDAttributes
- ActualAdmin/amd64
- ActualAdmin/arm64
- ReplayPreservesForeign
- HeaderRefusal
- BodyRefusal
- WithheldEOFSignal
- UnknownOutputRetainsGraph
- UsageBeforeRoot
- SourceCloseBeforeMutation

Separate namespace scenarios unsafe_owner, missing_opt, symlink_opt, regular_opt,
nonroot, unsafe_root_owner and unsafe_root_mode each require
TestProbeBinaryStageHostSandbox/RefusesParent/<scenario>.
All mandatory leaves AND top-level must pass exactly3 repetitions, zero skips or
failures, normal container/test exit0. No privileged container, no host
root/fixture bind, no Docker socket mount. Only the separately approved root-owner
setup starts with CAP_CHOWN, dropped before reexec/testing; all other containers
start with all capabilities dropped. Protected fixtures are read-only; private
/tmp and /opt are size-bounded. Namespace root is test-only, not host root.

Require graph/pinned bytes/marker/modes/hash, nine-field receipt, normal exit0;
refusals exact normal exit1/fixed safe diagnostic, usage2. Real stdin EOF withheld
must prevent mutation until signal, followed by joined normal refusal not timeout
kill. Conflict preserves foreign marker/inode/mtime. Source Close precedes first
mutation. Output failure retains graph and emits Unknown (not not_started).
Parent FD_CLOEXEC and real UID/mode/type come from kernel, not mocked metadata.

## Worker B — strict JSON acceptance parser only

ONLY NEW scripts/m8_binary_stage_host_acceptance.py and
scripts/test_m8_binary_stage_host_acceptance.py. Python stdlib only, verification
not application. No commands/tests/network/credentials/home/spawn/git. Read this
contract only as sufficient shared input. Main owns sandbox runner and actual
Go tests. Interface:

```python
def audit_events(text: str, scenario: str, repetitions: int, process_exit: int) -> dict:
    ...
```

CLI --go-json PATH --scenario (positive or seven named negatives above)
--repetitions N (positive int) --process-exit N; return0 ONLY passed, emit compact
JSON without raw log content/private diagnostics. Unknown/absent/invalid inputs
refuse nonzero. Only package github.com/fiztoz/uptime-phoenix/internal/bootstrap.
Parse each nonblank line as a JSON object with valid Action and typed event
fields. Reject any malformed/unrelated package, any fail/skip, panic/race/timeout/
no-tests output, nonzero process exit, missing/excess expected leaf/top counts,
zero/parent-only coverage. Require package completion pass and every required
leaf + top exactly N passes. Track actual run/pass lifecycle; no fabricated pass
without run, duplicate terminal, pass before run or unmatched pending run. Parent
subtest events are allowed but do not substitute required leaves. Error results
fixed reason codes, never raw e.Output or parser/filesystem exception messages.
Tests synthesize events for valid 1/3 repetitions, every missing guard and bad
shape/type/foreign package/nonzero/skip/failure/panic/parent-only/zero count; no
mock or authored parser tests are reported as executed by Worker B.

## Verification and limits

Main freezes full source/inventory and prior entries, runs native focused/full
race/build/vet/format, Linux CGO0 builds, actual authorized isolated Linux matrix,
strict parser tests, targeted assertion mutants + untouched controls, unchanged
release/parser/shard harnesses. Source drift means stop/refreeze/repeat; preserve
failed/interrupted/superseded evidence. Real MariaDB unchanged/unconfigured is
not accepted engine coverage. Native wrappers and Linux non-race process effects
are separate. No new host-authority, SSH delivery, remote termination or installed
success proof. No network/pulls/deploy/push. New continuation remains uncommitted
unless separately requested. Ownership/identity/no-open-user checks precede only
task-owned cleanup; old writer workspace/shared VM/cache remain untouched.

### Reproducible gate and verification-fix record

See [TESTING.md](../TESTING.md#m8-fixed-linux-host-local-binary-launcher) for the
committed fresh-checkout commands. `--allow-local-docker` is explicit consent for
these disposable local fixtures, not a VM start, remote endpoint or deployment.
Every create uses `--pull never`; imports consume only task-authored tar stdin.
Only all eight scenarios can produce passed=true/exit0. A single `--scenario`
is diagnostic-only, with passed=false/nonzero even when diagnostic_passed=true.
The stdout verdict is not enough: inspect result.json full_matrix, every strict
scenario verdict, preserved baseline resources and cleanup_errors as well.

Canonical verification-fix record (not production authority or a claimed remote
installation RCA):

1. **Missing top-level completion.** Initial parser control ran67tests with5
   failures. The implementation required leaves but omitted TOP_LEVEL from the
   required tuple, contradicting the contract and its own tests. Pdb and a
   one-variable in-memory differential established that a stream missing top-level
   passes was incorrectly accepted while valid complete input remained accepted.
   Requiring `(TOP_LEVEL,) + leaves` fixed the guard; control-02 passed all67.
   This was caught before relying on Linux acceptance, not dismissed as a fixture.
2. **Unavailable local engine.** First sandbox attempt stopped before creating
   its work directory or any Docker resources: the configured local socket was
   absent. Local context metadata confirmed no available Unix socket; no remote
   context was contacted and no VM was started by the agent. The user started
   their engine and authorized retry. The failure remains retained.
3. **Docker root-inode normalization.** linux-control-02 passed the positive and
   five original refusal namespaces, then correctly refused its own invalid
   unsafe-root-owner fixture: Docker import had normalized the root owner. This
   was not a launcher failure or root-negative proof. The user separately approved
   private writable root layers, setup-only CHOWN for the owner case, read-only
   task volume, capability clear and test reexec. Actual `/proc` zero-capability
   and no-new-privileges checks now precede helper invocation. No host root is
   changed and none of the refusal assertions was removed or weakened.
4. **Capability spelling.** linux-control-03 stopped at isolation inspection
   because the Docker daemon reports CAP_CHOWN while CLI accepts CHOWN. Only the
   optional CAP_ prefix is normalized; exactly the approved capability remains
   allowed, with kernel zero-cap checks after setup. linux-control-04 then passed
   all eight namespaces with owned images/containers/volume cleaned.
5. **Final policy hardening/refreeze.** The first frozen campaign caught9 intended
   mutants and completed its Linux matrix, but its full native race run was
   deliberately interrupted/retained before acceptance. Main found that using an
   existing imported image ID did not explicitly forbid Docker's default pull
   fallback if the image disappeared. Added `--pull never`; no completed run had
   actually pulled. Also made subset diagnostics fail closed rather than relying
   only on full_matrix=false. Added independent policy/matrix controls. Only the
   identified verifier group was terminated/joined; all gates were refrozen in
   run-02 even though launcher Go bytes were unchanged.

Evidence root: `tmp/m8-binary-stage-host-verification/`. Failed controls,
original parser/debugger differential, invalid fixture/isolation attempts,
first interrupted campaign and assertion mutations are retained, never counted
as final acceptance. Reviewers requested Grok4.7 and DeepSeek4.1-flash; inspection
reported no blockers, executed no gates and did not verify serving-model identity.
The subset-verdict observation was subsequently hardened by main. MiMo2.6Pro and
DeepSeek authored disjoint files only; main owns all executed verification.

The first run-02 policy mutant removed list entries and caused the AST policy
control to raise ValueError rather than an intended assertion. It was excluded,
not counted as a catch. The corrected single-value mutant changes never -> missing
without executing Docker. All13mutants were rerun with v2 names; application/test
source remained byte-identical. Six already-passed build/vet/Python/artifact/
cross-build stages were reused for those identical frozen inputs. The original
harness failure, invalid overlay and logs remain; no assertion was weakened.

## Executed final bounded verification

Accepted source: **24cdfe4eb6ab7265fe7ff021981bce1239b783c538209bb819d377b1f20e9786**,
**1,535 source entries**. All1,525 prior entries other than main.go remain identical
to the inherited-fd checkpoint; main.go is the only old Go file changed. Five new
Go files and four new Python verification files are included. The continuation
is18uncommittedpaths (10code,8docs), index empty, HEAD718e3b8. No new commit/push/
deployment is implied. Source, documentation/configuration manifests and exact
completion bytes are separately bound by the final metadata audit.

| Executed gate | Result |
| --- | --- |
| Offline Go build / vet | passed; 7.559s / 2.919s |
| Artifact native race repeat x3 | 2,847 named passes; 42.551s; no skips/failures |
| Linux CGO0 probe+admin cross-builds | amd64 3.161s; arm64 3.080s; not runtime acceptance |
| New Python parser/runner controls | 77 tests passed |
| Intended mutation checks | 13 caught; 39 untouched native-control passes |
| Actual isolated Linux/arm64 namespace matrix | all8scenarios x3; 78 named passes, including75required leaf/top passes plus3ActualAdmin parent passes; zero skips/failures; 31.589s |
| Full native race, count1, timeout3600s, p4 | 6,702 named passes,416staging; 3,520.884s; zero failures/malformed JSON/stderr |
| Full inventory/skip audit | same36packages:23passing+13no-test; all6,689 prior names retained;13new;547skips=542missing MariaDB DSN+5known optional/helper; no unknown skips |
| Focused native race repeat x3 | 381 passes;12.685s; no skips/failures |
| Existing release/backend harnesses | 24+4+27+17 controls passed |

The long polling tool timed out, but the independent verifier subsequently
completed with normal exit0; completion/result/coverage records, not that polling
response, establish acceptance. No duplicate verifier was started for the timeout.
Linux uses non-race static test/admin binaries in task-authored scratch images,
Docker client29.9.2/server29.5.2, network none and no host binds. Actual kernel
namespace root/parent and post-setup capability state are tested; both Linux ELF
architectures are staged as data, not executed. No Linux-amd64/OpenSSH runtime,
new MariaDB/broad M7, publisher/runtime identity or remote-host authority proof.
Missing golangci-lint/govulncheck remain missing; vet/format are not those scans.
Unchanged frontend and Helm gates were not run.

A first metadata audit stopped because its caller incorrectly treated the parser's
counts summary as a per-leaf counter map. The actual contract has repetitions,
package_passes, required_leaves/required_leaf_passes and test_runs/test_passes;
checks were corrected to positive11x3=33required/36named and negatives2x3=6,
with one package pass each and78named total. No application/test source or
acceptance condition changed. metadata-audit-correction.json retains the finding.

Authoritative evidence is run-02/{verification-result,coverage-audit,mutation-result,
cleanup-result,final-audit}.json under the evidence root, selected by CURRENT.json.
Final cleanup status comes from cleanup-result and the **post-cleanup** final-audit,
not passing tests. Only exact identity-verified/no-open-user task scratch may be
removed; logs, invalid/failed/interrupted evidence and frozen completion bytes
remain. The earlier writer's deferred .work is excluded, with no handle recheck,
VM stop or shared-cache/dependency prune. Remote delivery/launcher authorization,
current-host fencing and authenticated enrollment plus exact activation stay open
at this historical checkpoint.

## Subsequent bounded delivery slice

This host-local launcher is locally committed as `ac9c126` (not pushed). The later
[one-shot pinned SSH sender](M8_BINARY_STAGE_SSH_DELIVERY.md) calls the trusted,
preinstalled absolute launcher and verifies its receipt plus normal exit0; actual
isolated Linux/arm64 OpenSSH effects are separate later evidence, not an extension
of the historical launcher campaign above. That adapter is uncommitted and unwired.
It neither installs/authenticates the helper's file provenance nor establishes
current-host fencing, remote reaping, applied resources, enrollment or activation.
The later [restricted-key policy fixture](M8_BINARY_STAGE_SSH_AUTHORIZATION.md)
checks native OpenSSH action restrictions with generated controls in isolation;
it does not install real account policy or authenticate the production helper.
