# M8 — restricted OpenSSH staging-key authorization

## Goal and simpler reuse

Continue the verified [one-shot SSH sender](M8_BINARY_STAGE_SSH_DELIVERY.md)
without a concrete executor, DB grant, host fence, installation or production
caller. Establish a repeatable, actual OpenSSH acceptance contract for an
operator-protected **restricted root key** calling the existing preinstalled
fixed helper.

OpenSSH already supplies a per-key forced command and restrictions. A new
`stage-binary-authorized` wrapper would not strengthen the fixed action boundary:
a malicious key holder could request the accepted original command anyway. This
slice adds **no wrapper, verb, command parser, authorization port or production
source change**. It uses native policy and tests its real effects instead.

## Operator trust and fixed policy

The intended authorized_keys entry is one line with exactly the existing command:

```text
restrict,command="/usr/local/bin/phoenix-probe-admin stage-binary-host" ssh-ed25519 <operator-managed-public-key>
```

The public-key placeholder is illustrative, not an installer or generated key.
Never put a private key in this file. The operator protects this entry/file and
its ancestors, the absolute helper and its ancestors, sshd configuration, root's
configured shell/startup files and other root accounts/keys. No code in this
slice writes a real account or installs policy.

The intended server profile uses `PermitRootLogin forced-commands-only`, key-only
authentication, `PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
`PermitUserEnvironment no`, no accepted client environment variables, no overriding
server/Match ForceCommand and no unreviewed key options. Other account/session
policies remain operator-owned. `restrict` does not establish helper provenance,
remove root shell startup behavior, override arbitrary server environment policy
or create a database authorization decision. A pinned host key authenticates an
endpoint, not these configuration facts or physical host identity.

**Forced command is not original-command validation.** OpenSSH ignores a client's
requested exec/shell/subsystem program in favor of the configured command. A
client requesting a different exec or a shell can still provide a valid stage
frame and cause staging. This is the documented limited capability, not a refusal
or a general root shell. No `SSH_ORIGINAL_COMMAND` value is trusted or parsed.
Normal SSH may invoke the fixed command through root's shell; no direct-execve
claim is made.

The sender remains unchanged and still requests only
`/usr/local/bin/phoenix-probe-admin stage-binary-host`. It borrows its snapshot,
owns one exact/EOF-checked reader, and requires the matching nine-field receipt,
normal exit0, joined local resources and live context. Exec-attempt failures remain
Unknown, with no retry or remote termination/cleanup claim. This key policy does
not turn an acknowledgment into artifact_directory/applied, ownership, current DB
authority, host fencing, installation, enrollment or exact activation.

Possession of this key permits the fixed fresh-staging action for any valid supplied
intent/binary, not just a current reservation. The helper does not authenticate
header owner/attempt/client-key fields against the SSH principal or a database.
This slice adds no per-operation ACL, artifact allowlist, admission quota or flood
protection; repeated fresh stages can consume storage/CPU. Fresh-only conflicts
and bounded individual inputs do not solve those authority/admission requirements.

## Frozen implementation scope

- Extend only the existing Linux integration fixture with a private optional
  pre-start configuration callback. Existing `newSSHStageFixture(t, forced)`
  defaults remain unchanged. The callback receives the generated fixture and
  protected private key directory, never a real account/key/config path.
- New `ssh_binary_stage_authorization_linux_integration_test.go` owns the native
  restricted-key cases and generated unrestricted control key. No production Go
  source changes, no new external dependencies or migrations.
- Factor the existing strict JSONL acceptance parser into a shared named-contract
  implementation while preserving its old fixed `audit_events` API/default matrix.
  Add a separate fixed authorization contract wrapper and negative-control tests.
- The existing sandbox runner gains only an explicit `--authorization` full-matrix
  selector. It fixes the top test, extra opt-in environment and parser as one unit;
  there is no caller-supplied test/required-name override. Its default sender gate
  must still execute the old complete contract unchanged.
- Main owns all source integration and executed verification. Qwen Token Plan
  delegation was attempted but unavailable due harness/provider startup errors;
  no model review or worker execution is credited.

## Actual-effect acceptance

Dedicated top: `TestSSHBinaryStageAuthorization`.
Require each of these **12 exact names** three times:

- `TestSSHBinaryStageAuthorization`
- `TestSSHBinaryStageAuthorization/Fixture`
- `TestSSHBinaryStageAuthorization/Sender/amd64`
- `TestSSHBinaryStageAuthorization/Sender/arm64`
- `TestSSHBinaryStageAuthorization/ForcedExec`
- `TestSSHBinaryStageAuthorization/ForcedShell`
- `TestSSHBinaryStageAuthorization/RestrictedSubsystem`
- `TestSSHBinaryStageAuthorization/ControlCommand`
- `TestSSHBinaryStageAuthorization/ControlSubsystem`
- `TestSSHBinaryStageAuthorization/Forwarding`
- `TestSSHBinaryStageAuthorization/ForcedOnlyRootLogin`
- `TestSSHBinaryStageAuthorization/ReplayPreservesForeign`

Only the extra parent `TestSSHBinaryStageAuthorization/Sender` is allowed:
**36 required completions / 39 named passes / one normal package completion**.
Reject missing/extra/wrong-case/wrong-package/parent-only/subset/zero-selection,
skips/failures, malformed/truncated lifecycle, abnormal exit, panic/race or timeout.
Counts alone do not substitute for per-iteration ordering and completion.

Each fixture has two generated keys in the same protected authorized_keys file:
restricted forced-command key and unrestricted control key. The ordinary control
profile allows TCP forwarding globally and includes `Subsystem sftp internal-sftp`,
while retaining global PTY/agent/X11/tunnel/environment restrictions. This is
private test configuration only, not a recommended production control account.

- Both existing sender architecture cases must reach the unchanged helper and
  verify actual graph bytes/hash/size/modes with borrowed snapshot ownership.
- ControlCommand proves the generated unrestricted key can execute the exact
  namespace-local shell canary command. ForcedExec uses that same command with a
  valid frame but requires an actual staged graph/receipt and no canary.
- ForcedShell supplies a valid frame to a shell request; only the fixed helper's
  actual graph and receipt may result. Do not claim the shell request is rejected.
- ControlSubsystem proves a real SFTP v3 version exchange with the unrestricted
  key, with no file operations. RestrictedSubsystem supplies the same packet and
  requires normal exit1 without an SFTP response; OpenSSH intentionally ignores
  subsystem stderr. It then supplies a valid stage frame through a fresh subsystem
  request and requires exit0, the exact helper receipt and actual graph. This
  identifies the forced helper rather than accepting an arbitrary exit1 command.
- Forwarding proves an actual loopback echo through the unrestricted key first,
  then requires an explicit SSH prohibited-channel refusal for the restricted key
  under the same globally permitting configuration. Listener, clients and handler
  join. Denials from existing global PTY/agent/X11 settings are not credited to
  `restrict`.
- ForcedOnlyRootLogin independently checks effective `sshd -T` root/auth/command
  policy. The restricted sender must stage; the listed generated key without a
  forced command must receive no session and produce a **new privileged-monitor
  root-login refusal** in the protected fixture log. A setup failure, unexplained
  EOF, child-process log message or stale refusal does not pass. OpenSSH's preauth
  child receives placeholder options and can send client-visible authentication
  success before the privileged monitor rejects root; a `NewClientConn` return
  alone is not a login grant. No arbitrary control command runs in this profile.
- Replay preserves deliberately foreign marker bytes/inode/mtime. It does not
  establish every graph inode's identity or cleanup/adoption authority.

Only task-owned isolated Docker/OpenSSH fixtures and generated keys are used,
within the previously approved boundary. Cached digest image only, runtime
`--pull never`, private empty Docker credentials, private PID/network namespace,
PID1/kernel capability/no-new-privileges verification, no host binds/volumes/added
host devices or privileged mode, read-only root, bounded noexec tmpfs/CPU/memory/PIDs, only
SETUID/SETGID/SYS_CHROOT/KILL. Namespace-local control shell writes and loopback
forwarding are test effects, not live-host access. Probe ELFs remain data. No new
capability, download, root-home/key access, VM start/stop or shared pruning.

## Draft-boundary regression record

**Summary.** The unreleased authorization fixture initially asserted two incorrect
OpenSSH boundaries: subsystem stderr forwarding and client-visible rejection of
an unforced root key during `NewClientConn`. Both assertions failed in every one
of three repetitions in actual runs 01, 02 and 03. These were test-model defects,
not a production sender/helper fix or evidence of a customer incident. The final
implementation is the frozen working-tree source below, not a new commit.

**Symptoms.** RestrictedSubsystem reported `restricted subsystem did not become
an honest fixed-helper frame refusal`: the actual result was normal exit1, zero
stdout and zero stderr. ForcedOnlyRootLogin reported `root key lacking a forced
command was not refused at authentication`: the client value existed despite a
root-refusal log, but attempting a session failed before any control command ran.
Protected key parsing proved distinct restricted/control keys and exact options;
`sshd -T` independently proved the intended effective root/auth/command settings.

**Root cause and mechanism.** OpenSSH `do_exec_no_pty` supplies `s->is_subsystem`
as `ignore_fderr` to `session_set_fds`, so the helper's ordinary stderr does not
reach a subsystem client. Separately, `sshauthopt_serialise(..., untrusted=1)`
sends placeholder `force_command="true"` to the preauth child. That child's
`auth_root_allowed` can succeed while the privileged monitor, using real options,
rejects the unforced root key. Client handshake success is therefore not a
privileged login/session grant. An early hypothesis of an unrestricted root grant
was falsified by session refusal, a new monitor denial and the upstream call path.

Public upstream mechanism references, fetched 2026-10-22 and retained in
`public-openssh-references.json` with the task evidence:
[option serialization](https://raw.githubusercontent.com/openssh/openssh-portable/master/auth-options.c),
[root policy](https://raw.githubusercontent.com/openssh/openssh-portable/master/auth.c),
[privileged monitor](https://raw.githubusercontent.com/openssh/openssh-portable/master/monitor.c),
[preauth completion](https://raw.githubusercontent.com/openssh/openssh-portable/master/auth2.c),
[session fd handling](https://raw.githubusercontent.com/openssh/openssh-portable/master/session.c).
These explain the observed mechanism; mutable upstream source is not pinned
binary/source correspondence or proof about a production server.

**Fix and validation.** RestrictedSubsystem now checks normal exit1/no SFTP VERSION,
then independently proves replacement by staging a valid frame through a fresh
subsystem request and verifying the exact receipt/graph. ForcedOnlyRootLogin
requires no accepted unforced session plus a new bounded-read privileged-monitor
refusal, excluding stale and child-process logs; a generic disconnect cannot pass.
The hardened effective policy and positive restricted sender must also succeed.
Run06 and the final-source controls pass these original cases three times. No
production implementation was changed to accommodate the fixture assumptions.

**Discovery and gaps.** Delve was unavailable; bounded semantic instrumentation,
actual session attempts and verified public source supplied the fail-path trace.
Mock receipts and client API labels did not exercise sshd's privilege-separation
or subsystem fd behavior. The fixture now owns raw subsystem channels and joins
request/stderr readers because `Session.RequestSubsystem` does not start Session
copies or make `Session.Wait` valid. A bounded output writer uses a named buffer,
not an embedded `bytes.Buffer` whose promoted `ReadFrom` could bypass `Write`.

**Retained verification corrections.** Draft run04's undefined diagnostic-helper
call was a compile/setup failure, not acceptance. A temporary control-02 invocation
used JavaScript `index` rather than `indexOf` and started no gate. The first Linux
mutation auditor correctly saw the forced-exec canary violation but falsely
rejected that catch because it treated the JSON converter's exit0 as the test exit.
The corrected campaign checks `run-openssh` exit1 separately from converter exit0,
then executes fresh controls and every effect mutant. Original failures are
retained, never rewritten as accepted gates. Source/test inputs did not change for
that metadata-only correction.

## Verification and status

The source is frozen at
`064b566500016ce9e7f691217270ced6928b9901558776ed8370d6704036ca56`
(**1,547 source inputs**), separate from the immutable prior sender checkpoint
`dcffab8b5266e3094a13e70c9f6c462e179dbd3865a923db249b01b74b623011`
(1,544 inputs, 17 uncommitted paths). This follow-up adds three source/test inputs
and changes only the shared test fixture/parser/sandbox among that baseline's
inputs. No production sender/helper, core, runner, schema, startup, HTTP or
configuration/dependency input changed. Docs are bound separately. The compiler
is installed Go1.26.9, offline/sanitized children and the exact CI embed placeholder.

| Executed gate | Result on final source |
| --- | --- |
| `go build ./...` | Passed, 18.669s |
| `go vet ./...`, Linux/arm64 provisioning vet and all-source gofmt | Passed; golangci-lint and govulncheck unavailable, not silently substituted |
| Artifact/domain/service/bootstrap/admin race, count3 | 3,018 passes, no skips/failures, 76.433s |
| Focused sender/parser race, count3 | 171 passes, no skips/failures, 5.294s; untouched final post-mutation control also 171 |
| Linux amd64 and arm64 probe/admin CGO0 builds | Passed, 13.808s / 7.220s; not Linux-amd64 runtime acceptance |
| Python old/new completion, selector and offline-policy controls | 36 passed; untouched post-mutation control also passed |
| Default actual OpenSSH, pre/post mutation | 24 required / 27 named completions, three repetitions, one normal package/process completion, no skips/failures/cleanup errors |
| Authorization actual OpenSSH, pre/post mutation | 36 required / 39 named completions, three repetitions, one normal package/process completion, no skips/failures/cleanup errors |
| Intended mutation controls | Six Python and five actual Linux effect mutants caught; normal assertion failures, never compilation/skip/panic/race/timeout/killed-process substitutes |
| Existing offline release/M6 harness controls | Passed: image24, chart4, parser27, shard17 plus shell/release controls; not broad operational/release acceptance |
| Full native race | 6,758 named passes, zero failures, 547 audited skips, 1,955.951s; all prior 6,758 native names retained, 23 test packages + 13 no-test packages |
| Source/config/docs audit and task scratch cleanup | 21 intended paths, clean index/format/whitespace, 336 local link targets and two added shell blocks syntax-checked; 19 exact owned scratch trees removed after identity/no-user rechecks; zero owned Docker resources; logs/frozen/failure evidence retained |

Actual execution is **Linux/arm64, OpenSSH_10.3p1 / OpenSSL3.5.9**, using static
CGO0/non-race test/admin binaries. Native race evidence is **Darwin/arm64**; it does
not execute the new Linux-only fixture. Both ELF architectures are staged data,
not executed probes. Source-level checks and mock receipts do not replace kernel/
OpenSSH effects. The actual mutants remove forced command or restrict, relax root
policy, substitute SFTP, or fabricate a sender acknowledgment. For the first three,
metadata-only option-count/effective-policy guards were deliberately adjusted in
the isolated mutant so they could not mask the actual effect assertion. The final
controls use untouched frozen source.

Evidence lives in `tmp/m8-binary-stage-authorization-verification/run-01/`:
source/mirror manifests, `verification-result.json`, `coverage-audit.json`,
`linux-campaign-result-02.json`, Python mutation results, original draft failures,
public source references and Qwen startup failures. The first campaign's
metadata-auditor refusal is preserved separately; only corrected actual-process
evidence is accepted. A cleanup-auditor false refusal of the fixture's credential-
empty `{auths:{}}` shape stopped before Docker/lsof/deletion; the narrowly corrected
empty-only check and all 19 fresh probe/apply observations passed. No source/test
input changed for either audit-only correction.

`final-audit.json` and the parent `CURRENT.json` bind verified-uncommitted source,
verification and completed cleanup separately. The retained source is now
`.overlays/frozen-source/`; final docs are separately bound in
`.overlays/completion-docs-final/`, configuration in `.overlays/completion-config/`.
Historical scripts referencing removed `.work/final-source` must not be blindly
rerun. The 19 removed workspaces are this task's outer scratch plus 18 explicitly
identified Linux phase scratch trees. Public base image and frozen/raw/failed
evidence remain. Earlier writer scratch is excluded: no new handle check, VM stop
or shared prune. Qwen delegation never reached a model; no model review or worker
execution is credited, and Pi was not repaired/reconfigured.

Together with the prior sender, **21 paths remain uncommitted**, index empty and
HEAD `ac9c1268d0843c2304e44aa453e9b1d6225948b8` unchanged. No commit, push,
deployment, real account-policy installation or fresh remote observation occurred.

The full native skips are 542 missing `TEST_MARIADB_DSN` plus five known optional/
helper skips. All 1,535 committed source inputs remain byte-identical. Default
MariaDB skips remain skips, not new engine coverage. Broad M7, M6/release,
current-host authorization/fencing, concrete executor and enrollment/activation
remain open. Do not wire this policy to a journal `applied` report. No commit,
push, deployment or live account/policy installation is authorized here.
