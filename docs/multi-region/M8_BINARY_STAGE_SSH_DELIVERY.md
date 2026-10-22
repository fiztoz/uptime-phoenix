# M8 — one-shot pinned SSH binary-stage delivery

Continuation after `ac9c126` and the [fixed Linux host launcher](M8_BINARY_STAGE_HOST_LAUNCHER.md).
This bounded slice implements a real transport to that preinstalled launcher. It
is **not** an installation resource executor, runner integration, deployment,
current-host fence, cleanup protocol, enrollment or installation success.

## Frozen implementation contract

Add a concrete adapter in `internal/adapters/provisioning/ssh_binary_stage_send.go`:

```go
const SSHProbeBinaryStageCommand = "/usr/local/bin/phoenix-probe-admin stage-binary-host"
var ErrSSHBinaryStageInput = errors.New("invalid SSH binary staging input")
var ErrSSHBinaryStageUnknown = errors.New("SSH binary staging acknowledgement unknown")

type SSHBinaryStageAck struct {
    Intent domain.ProbeBinaryStageIntent
}

type SSHBinaryStageSender struct { /* private per-instance transport state */ }
func NewSSHBinaryStageSender(ctx context.Context, privateKey []byte,
    hostFingerprint, clientFingerprint string, policy probe.EndpointPolicy,
) (*SSHBinaryStageSender, error)
func (s *SSHBinaryStageSender) Send(ctx context.Context,
    intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary,
) (SSHBinaryStageAck, error)
```

These are adapter APIs, not a new core port/service/domain receipt. A future
concrete executor can call the adapter only after separately solving its actual
host-authority/fencing protocol. Do not add a speculative facade or wire this
adapter into the current runner, bootstrap, HTTP or CLI.

Reuse the existing fingerprint-only SSH constructor, exact client public-key
fingerprint check, endpoint policy, one resolution and numeric-IP dialing. The
sender may retain an unchanged `SSHBinaryStageInspector` privately to reuse its
constructor/validation and host connection. Do not modify existing inspection,
preflight, helper, preparer, Receive or writer implementations.

Before I/O, require a live non-typed-nil context, valid systemd intent, **SSH user
exactly `root`**, non-typed-nil prepared binary with exact `Pin()`, and both pins
matching the configured public fingerprints. Invalid intent/source/root/pin
bindings return zero ack and `ErrSSHBinaryStageInput`; constructor credential or
trust input retains the existing fixed SSH input sentinel. No raw diagnostics.
Encode the existing exact header with `EncodeProbeBinaryStageHeader`.

The snapshot is borrowed: never prepare, change or close it. Open at most one
owned reader. Opening before exec is preferred so a known unavailable source does
not start a helper. Own partial/error Open readers too; close exactly once. A
pre-exec reader/Open/Close failure returns the bare artifact storage sentinel.
Source Close must unblock an active Read; join active local reads and any running
cancellation callback before returning. No hard-stop guarantee for arbitrary
uncooperative implementations is claimed.

## Fixed remote boundary and trust preconditions

Attempt exactly one exec request for the constant above, with no caller command,
arguments, destination path, shell fragment, environment request, PTY, subsystem,
forwarding, sudo, password, SSH agent, fallback, upload or remote installation of
the helper. The SSH server may implement exec through the root account's shell;
no claim is made that the SSH protocol uses direct `execve`.

The operator must already trust and install the intended helper at the fixed
absolute path, its protected ancestor chain, root's shell/startup configuration,
and the SSH server/account/key authorization. A pinned SSH host key authenticates
the endpoint, **not** those files, the helper binary's provenance, a physical host,
current database authority or permission for a particular journal reservation.
No SFTP probe or label is substituted for these preconditions. Root/namespace
metadata is not a hostile-root/mount fence. No production caller is added here.

Use a fixed two-minute per-call transport context, shortened by the caller; set
the connection deadline. This is cooperative local work budgeting, not a remote
termination guarantee. SSH host trust must be checked before client authentication.

The stream is the unchanged header followed by exactly the pinned raw bytes and
real source EOF. Check counts before slicing, bound empty reads to 100, reset the
empty counter on progress, verify exact size and SHA256, and refuse multi-cause
EOF using the existing bounded single-wrapper terminal classifier where usable.
No second source open, body prefetch, compression, trailer, LimitReader-generated
EOF or generic io.Copy path that evades count/EOF checks.

After full source EOF and successful owned-reader Close, close SSH stdin to send
channel EOF. Every failure closes the transport before cleanup can send another
normal stdin EOF. Nevertheless, SSH disconnect can itself produce process stdin
EOF: **last-byte disconnect may cause remote staging**. Local cancellation or
channel close does not prove remote cancellation, termination, no effect or
cleanup. Preserve that uncertainty; never retry, redispatch or delete remotely.

## Acknowledgment, bounds and errors

`SSHBinaryStageAck{Intent: intent}` means only: the authenticated endpoint returned
one complete matching helper receipt, normal SSH exit status 0, locally joined
transport/source resources and a still-live context. It is not a local seal,
independent filesystem observation, ownership, `artifact_directory` application,
committed reservation, fence, installed state or runtime identity. Its shape can
be constructed by callers and grants nothing.

Receive at most **2048 stdout bytes** and **4096 stderr bytes**; over-budget output
closes the transport and fails. Never expose or persist raw channel diagnostics.
Drain stdout/stderr concurrently with upload through bounded writers; `Wait` must
join the SSH library's stream copies before success. Missing exit status, nonzero
exit, signal exit, partial output, extra byte/line, malformed receipt or context
revocation never acknowledges. No output/trust/error text is interpreted as a
retry or success signal.

The receipt is one UTF-8 flat JSON object followed immediately by one LF, with
exactly these nine canonical unescaped keys (order/JSON whitespace may vary):
`state`, `directory_name`, `marker_sha256`, `pinned_version`, `version_source`,
`os`, `architecture`, `sha256`, `size`. Reject unknown/duplicate/escaped spelling/
case-alias/missing/null/wrong-type fields, malformed/trailing JSON, internal LF,
invalid UTF-8, nested values and noncanonical integer spelling. Values must equal
`local_binary_staged`, the operation-derived relative directory, `MarkerSHA256()`,
the exact pin version, `operator_pin`, `linux`, pin architecture, digest and size.
Do not marshal a domain value or import bootstrap from this adapter.

Pure parser in `ssh_binary_stage_receipt.go`:

```go
func parseSSHBinaryStageAck(ctx context.Context, body []byte,
    intent domain.ProbeBinaryStageIntent) (SSHBinaryStageAck, error)
```

It returns only exact ack/nil or zero ack/`ErrSSHBinaryStageUnknown`, including bad
context or intent. It may reuse existing same-package canonical flat-key/integer
helpers; do not change them. The transport owns public type/error declarations.

Before attempting `Session.Start`, preserve only fixed SSH input/destination/
trust/connect, artifact storage and context sentinels. **Immediately before the
Start call, cross the uncertainty boundary. Every subsequent failure is zero ack
plus `ErrSSHBinaryStageUnknown`, including Start failure, source errors, close
errors, cancellation, SSH errors and a valid receipt accompanied by nonzero exit.**
The source and local SSH client/session/connection/callback/goroutines must join
before returning. `client.Wait`/local session joining is not remote process reaping.
Recheck context after local joining and before returning a successful ack.

## File ownership and scope

- Transport worker: new `ssh_binary_stage_send.go` and
  `ssh_binary_stage_send_test.go` only. It implements the actual sender, owned
  source/callback lifetime and mock/real-loopback tests as appropriate; no commands.
- Receipt worker: new `ssh_binary_stage_receipt.go` and
  `ssh_binary_stage_receipt_test.go` only; pure bounded parser/tests; no commands.
- Main: contract, integration tests, isolated OpenSSH verification runner/gate,
  documentation and all executed acceptance. No developing worker file is edited
  before worker handoff/join.
- Read-only review agents do not edit files or execute gates.

No new external Go dependencies, migrations, authorization dimension, monitor,
notification provider or default deployment dependency. No change to existing
read-only SFTP APIs or installation journal/runner behavior.

## Required verification and approved fixture boundary

Native tests must cover constructor/root/intent/pin/typed-nil/context refusal,
real SSH host-pin rejection before authentication, exact fixed exec request only,
header/body/real EOF, source ownership/count/hash/close/no-progress failures,
source cancellation and active-read/callback joins, bounded channel output,
receipt binding, missing/nonzero/signal exit, post-exec cancellation/Unknown and
no retry. A Go SSH server is protocol/lifetime evidence, **not OpenSSH shell or
actual fixed Linux helper acceptance**. Generated test keys only.

Dedicated opt-in Linux acceptance must exercise actual **OpenSSH sshd**, the
unchanged built admin at the fixed path, both architecture fixtures as data,
actual protected literal `/opt`, exact persisted binary/marker/modes, conflict
preservation, receipt-plus-exit, and last-byte/disconnect/uncertainty. Inspect the
actual effects, not only codes or mocked receipt production. Require exact named
leaves/repetitions/normal process and package completion; reject skips, zero
selection, parent-only passes, build errors, panic/race/timeouts and truncated
logs. A subset is diagnostic, never full acceptance.

The user explicitly approved task-owned isolated local Docker/OpenSSH fixtures
and, if needed, pinned public fixture image/package downloads. No host binds,
host `/opt`, live hosts, SSH user credentials, VM start/stop, shared-cache pruning,
remote deployment or probe execution is approved. Runtime network stays inside
the container namespace on loopback. Use only generated test keys and private
empty Docker credentials, digest-pinned images, no pull fallback on runtime
create, bounded resources, no privileged/host-network container, and only the
capabilities actually required by sshd. Track exact owned identities, join fixture
processes and clean only owned resources. A pull/download is a separate recorded
fixture acquisition, never runtime fallback or trust in a floating tag.

Freeze source bytes/inventory plus documentation/configuration separately. Run
build, full default race, focused repeated race, vet/format fallback, both Linux
CGO0 builds, existing harness regressions and intended assertion mutations.
Real-engine fencing/schema behavior is unchanged; default-suite MariaDB skips
are not engine acceptance. Report authored/executed/passed/skipped distinctly.
No frontend/Helm changes: those gates are not applicable. Preserve failed,
interrupted and superseded evidence. Older writer deferred cleanup is excluded.

## Evidence

Implemented locally after `ac9c126`, **uncommitted and not pushed**. Only six new
Go files and three new Python files change executable/test inputs; all 1,535 prior
source entries remain byte-identical. No existing helper, inspector, core port,
service, domain, runner, HTTP, startup, schema or deployment input changed.

Final source inventory: **1,544 entries**,
`dcffab8b5266e3094a13e70c9f6c462e179dbd3865a923db249b01b74b623011`.
Documentation and configuration are bound separately. The disposable source copy
uses the exact CI embed placeholder
`bf80fb300c511f60864cdff7fddcbcb71d4718ae25d1ecfcb1edb04e42e58182`.
Installed Go 1.26.9 ran with GOENV off, local toolchain, offline proxy/checksum
settings, private scratch/cache/home and no inherited DSNs or credentials.

| Executed check | Result and limits |
|---|---|
| `go build ./...` | Passed, 21.227s |
| `go vet ./...` and `gofmt -l internal cmd` | Passed; no unformatted files. golangci-lint and govulncheck were unavailable, not silently substituted |
| `go test -race -count=1 -json -timeout 3600s -p 4 ./...` | 6,758 named passes, zero failures, 547 audited skips, 1,959.047s; all 6,702 prior names retained plus 56 new names; 23 test packages and 13 no-test packages |
| Repeated artifact/domain/service/bootstrap/admin selection, with explicit SSH sender/parser selection | 3,018 race passes, zero skips/failures |
| `SSHBinaryStage\|ParseSSHBinaryStageAck`, count3/race | 171 passes, zero skips/failures; untouched post-mutation control also passed |
| Both Linux amd64/arm64 probe and admin builds | Passed with CGO0; builds are not Linux-amd64 runtime acceptance |
| Python SSH negative controls | All 19 passed; malformed lifecycle, missing required completion, skips/failures/subsets, abnormal process/package exit and unsafe fixture policy cannot pass |
| `make release-image-gate m6-backend-harness-gate` | Passed offline, including image24, chart4, parser27 and shard17 unit controls plus shell/release controls |
| Intended assertion mutants | 23 caught: 18 native transport/receipt, three Python gate, two actual OpenSSH/Linux effect mutants; normal intended assertion failures, not compile errors, skips, panic/race, killed process, zero selection or Go deadline timeout |
| Actual pinned OpenSSH/Linux-arm64 | Final and untouched post-mutation runs each passed 27 named checks / 24 required completions / one package completion across three repetitions, no skips/failures and no Docker cleanup errors |

The 547 default skips are **542 missing `TEST_MARIADB_DSN` plus five known
optional/helper cases**. No new MariaDB acceptance was executed, and broad M7
remains open. Native race runtime is Darwin/arm64. The Linux fixture uses static
CGO0/non-race test/admin executables, OpenSSH_10.3p1 with OpenSSL 3.5.9, and the
approved cached arm64 image
`docker.io/linuxserver/openssh-server@sha256:d9b26e329e4ca14a1dd57029c4b18bf31035ef2bd9a573abe7fc5781e71a9739`.
Public acquisition was separately recorded using empty Docker credentials; runtime
creates use `--pull never` and do not start the image's normal entrypoint.

The runtime verifies private PID1/nonce/kernel capability state, no-new-privileges,
network none with only namespace loopback, read-only root, no host binds/volumes,
bounded noexec tmpfs and exactly SETUID/SETGID/SYS_CHROOT/KILL capabilities. Only
sshd, its shell, the test and built admin execute. Both probe architectures remain
data; modes/noexec mounts prevent staged-probe execution, so the absent canary is
not independent evidence of no attempted exec. Generated host private keys exist
only inside protected disposable namespace storage; the generated client key is
in memory. No real host/account credential is read.

Positive cases use ordinary SSH exec to the literal helper with invalid hub
configuration inherited from sshd. They verify actual directory/binary/marker
bytes, hash/size and modes. Replay checks a deliberately foreign marker's
bytes/inode/mtime, not an independently measured identity for every file. The
unchanged fresh-only writer has its separate broader filesystem acceptance.
PostStageNonzero and PostStageLostReceipt run the real helper before altering
only status/output and require the retained graph with zero ack/Unknown. The
last-byte case logs observed directory presence after fixture-owned teardown;
it does not assert absence or completeness. Fixture PID1 teardown is not a sender
remote-termination guarantee.

Read-only implementation and test-effect reviews reported no blocking findings;
they are inspection, not executed acceptance, and do not authenticate model
serving identity. The test reviewer reported read-only inspection commands despite
the no-command request; no reviewer execution is credited as gate evidence.
Main independently executed and audited the checks above. No frontend or Helm
input changed, so their gates were not run.

Evidence is retained under `tmp/m8-binary-stage-ssh-verification/`: source/mirror
manifests, verification-result and coverage-audit, mutation-result,
extra-mutation-result, linux-mutation-result, final/post-mutation Linux results,
raw logs and failed/superseded drafts. Cleanup and final audit are separate from
passing tests; the final pointer records their completed status. Earlier writer
VM-held scratch is excluded, with no handle recheck, VM stop or shared prune.

## Subsequent restricted staging-key fixture

The later [restricted-key policy acceptance](M8_BINARY_STAGE_SSH_AUTHORIZATION.md)
reuses this unchanged sender/helper and the approved isolated Linux/OpenSSH fixture.
Native `restrict,command=...` policy replaces requested exec/shell/subsystem actions;
it is not an original-command validator or a current-reservation grant. Generated
unrestricted controls distinguish per-key TCP forwarding refusal from global
settings. Root-policy evidence uses the privileged monitor and denied session,
not a client handshake alone. The shared fixture/parser/sandbox gain only private
configuration and a fixed whole-matrix selector; the default sender gate must still
pass. This follow-up's source/evidence are separate from the historical checkpoint
above, remain uncommitted, and install no real key policy or concrete executor.

## Draft-boundary regression record

**Summary.** The unreleased draft sender had six reproducible source-lifetime and
completion defects. Main added direct boundary tests and fixed only the new
sender; no released helper/receiver/writer behavior changed. The fix identity is
the frozen working-tree source above, not a claimed new commit or production fix.

**Symptoms and root cause.** `native-boundary-repro-01` reproduced all six logical
boundaries three times: typed-nil partial Open cleanup panicked; pre-exec reader
Close failure was hidden behind destination/transport error; full `(n, io.EOF)`
was read again; known cancellation after source Close still sent normal stdin
completion; known cancellation before Start still attempted exec; nil-error short
writes were retried. `stageSendSource.close` did not distinguish typed nil,
`stageSendCall.join` discarded close status, `stream` forgot terminal EOF, and
`execute` lacked the two effect-enabling context checks. The write helper treated
an invalid short write as progress rather than failed transport.

**Fix.** Typed-nil cleanup is safe; once-owned Close status reaches public
pre-exec storage classification; final real EOF is retained; context is checked
before the Start attempt and before channel EOF; short/count-invalid writes fail
closed. Running source/cancellation work and local SSH copies join, and context
is rechecked after cleanup. Later failures still produce only zero ack/Unknown.

**Fixture corrections and discovery.** Original Go-server assertions confused
server-side EOF on disconnect with an explicit sender channel EOF. Repeated
repros disproved that interpretation; direct local stdin instrumentation now
checks actual completion ordering. No Unknown protection was removed. A Python
negative control also reproduced acceptance of an `https://...@sha256:...` image
reference; conditional pdb/source tracing isolated the scheme check, which now
rejects URL schemes while preserving registry ports. Initial Linux runs honestly
failed for an omitted offline module-cache argument and the image packaging
`sshd.pam` rather than `sshd`; corrected invocation and the observed binary path
were verified without downloads/fallback or normal image startup. Delve was
unavailable; source/knob tracing and reproducible differential cases supplied the
native fail-path evidence.

**Why the draft checks missed it.** Synthetic receipts are useful transport
fixtures, but the initial checks omitted independent cleanup/completion boundaries
and inferred an EOF cause the server cannot distinguish. These were draft review
and fixture gaps, not evidence of a customer incident. Native direct boundary
models and actual OpenSSH/persisted-effect cases now have distinct claims.

**Validation.** Original boundary repros pass in native-control-03, final focused
and post-mutation race runs; all executed gates in the table passed on the frozen
source. The actual Linux mutants prove that ignoring exit status fails the real
post-stage case and returning a fabricated acknowledgment fails actual graph
checks. Full native race and isolated Linux-arm64 are verified; production helper
provenance, Linux-amd64 runtime, current-host fencing, remote termination/cleanup,
installation, enrollment and exact activation remain unverified or unimplemented.
