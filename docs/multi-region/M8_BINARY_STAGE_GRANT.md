# M8 — one-shot stage grants: DB admission, host verification, authorized delivery

Continuation after the [one-shot SSH sender](M8_BINARY_STAGE_SSH_DELIVERY.md) and
the [restricted staging-key fixture](M8_BINARY_STAGE_SSH_AUTHORIZATION.md). This
record covers three increments that together tie one journal artifact attempt to
at most one remote staging process. It is **not** a concrete executor, runner/HTTP/
startup wiring, persistent key custody, policy installation, cleanup, enrollment
or installation success.

| Increment | Commit | What it adds |
|---|---|---|
| A — DB admission + signer | `19fb9cc` | Irreversible consumption of one exact `artifact_directory` attempt (migration `085`), then an injected Ed25519 signature over a domain-separated message |
| B — host verification | `6221326` | `phoenix-probe-admin stage-binary-authorized`: trusted key, fresh nonce challenge, grant check before any payload byte or filesystem effect |
| C — authorized delivery | `6221326` | `SSHBinaryStageSender.SendAuthorized`: challenge → one `Authorize` call → header + grant frame + payload |
| D — authority key custody | working tree | `phoenix-probe-admin stage-authority <init\|check\|public-key>` and `bootstrap.LoadProbeStageGrantSigner` over the shared protected 32-byte key reader |
| E — artifact executor | working tree | `provisioning.SSHArtifactDirectoryExecutor`: the install runner's `artifact_directory` resource via `SendAuthorized` |

Increment A's verification is in [TESTING.md](../TESTING.md#m8-one-shot-stage-admission-increment-a-not-host-authorization)
and the [status](IMPLEMENTATION_STATUS.md) record; it is summarized, not repeated, here.

## Why this is different from the rejected wrapper

The restricted-key record rejected a `stage-binary-authorized` *wrapper* that
would merely re-validate the requested SSH command: a key holder could request
the accepted command anyway. This verb is different. It checks a **signature the
key holder cannot produce**: the hub's authority key signs only after the DB
consumes the exact attempt, and the signature covers a nonce this helper process
chose. Holding the SSH staging key is no longer sufficient to stage.

## Protocol

```text
helper (root, host)                         sender (hub)                 hub DB / signer
 load /etc/phoenix-probe/stage-authority.pub
 acquire fixed /opt parent
 nonce ← crypto/rand (32 bytes)
 stdout: challenge line ───────────────────▶ parse (exact bytes)
                                             request = {intent, key id,
                                               nonce, intent.MarkerSHA256()}
                                             Authorize(request) ───────────▶ consume attempt once
                                                                ◀─────────── Ed25519 grant
 stdin ◀──── header ‖ grant frame ‖ raw pinned binary ‖ EOF
 decode header → intent
 read 72-byte grant frame; verify over
   (trusted key id, own nonce, intent marker)
 └─ refuse ErrProbeBinaryStageWriteUnauthorized (nothing created)
 unchanged Receive → fresh-only writer
 close parent; stdout: receipt line ──────▶ challenge prefix + exact receipt + exit 0
```

Wire formats (`internal/adapters/provisioning/binary_stage_grant_frame.go`):

- **Challenge line**: exactly
  `{"state":"stage_challenge","authority_key_id":"<64 hex>","nonce":"<64 hex>"}` plus LF
  (189 bytes). Parsing accepts only these canonical bytes; a zero nonce is refused.
  It carries no marker because the helper has not read the intent yet.
- **Grant frame**: `PHXGRT1\n` + raw 64-byte signature. The frame carries no request
  fields; the helper rebuilds the request from its own nonce, its trusted key and
  the decoded header, so nothing in the frame can redirect what was signed.
- **Trusted key file**: 64 lowercase hex characters (raw Ed25519 public key) plus LF.

## Host contract (increment B)

`internal/bootstrap/probe_binary_stage_authorized.go` and
`probe_binary_stage_authority_linux.go`.

- No arguments, flags or paths. Before the challenge (so a sender never burns a
  one-shot admission for a helper that cannot stage) it requires effective uid 0, the
  trusted key, this host's sshd host-key fingerprints, entropy and the protected
  `/opt` parent.
- **Host binding.** Host keys are read from `ssh_host_{ed25519,ecdsa,rsa}_key.pub`
  under the protected `/etc/ssh` (same descriptor walk and file checks as the
  authority key, at most 16 KiB each; an absent file is skipped, but a present file
  that fails a check refuses the whole set). After the header decodes, the intent's
  pinned `host_key_fingerprint` must be one of them, or the helper refuses
  Unauthorized **before reading the grant frame**. This closes the cross-host relay:
  a compromised host A cannot feed host B's challenge through the hub for A's own
  attempt, because B refuses an intent pinned to A's key. An sshd using other
  `HostKey` paths fails closed.
- The key is read through a no-follow descriptor walk `/` → `etc` → `phoenix-probe`;
  each directory root-owned and not group/world writable. The file must be a regular,
  root-owned, non-group/world-writable, single-link file of exactly 65 bytes
  (opened `O_NONBLOCK` so a planted FIFO cannot block). Non-Linux always refuses.
  These checks are not key provenance: the operator installs the right key.
- After the host binding passes, the hook reads only the
  fixed grant frame and calls `ProbeBinaryStageGrant.ValidFor(trustedKey)` on the
  rebuilt request. A non-root intent fails because the admission request requires root.
  An invalid frame or signature is the definite pre-creation
  `ErrProbeBinaryStageWriteUnauthorized`. A stream ending early is
  `ErrProbeBinaryStageWriteInput`. Neither creates anything.
- After verification, the unchanged Receive/writer/receipt path runs. `stage-binary`
  and `stage-binary-host` are unchanged.

**Operator policy:** force the staging key to the authorized dispatcher, otherwise
the legacy unauthorized verbs remain reachable by any key holder:

```text
restrict,command="/usr/local/bin/phoenix-probe-admin authorized-dispatch" ssh-ed25519 <operator-managed-public-key>
```

`authorized-dispatch` runs only `stage-binary-authorized` or
`stage-host-action-authorized`, by exact `SSH_ORIGINAL_COMMAND` match; see the
[host-action record](M8_HOST_ACTIONS.md#one-forced-command-authorized-dispatch).

## Sender contract (increment C)

`internal/adapters/provisioning/ssh_binary_stage_authorized_send.go` plus an
authorized mode in the shared `stageSendCall` (`ssh_binary_stage_send.go`).

- Pre-exec validation and sentinels are identical to `Send`. A nil authorizer is
  `ErrSSHBinaryStageInput` before any connection.
- After exec the sender waits for the first stdout LF, helper exit, or the deadline.
  stdout must be exactly one canonical challenge line at that point. The sender
  refuses to call `Authorize` if the helper has already exited or if less than 30 s
  of the two-minute per-call budget remains (so a slow connect never spends the
  admission without time to deliver it). It checks for helper exit again after
  `Authorize` returns. `Authorize` is called at most once and never retried. The
  returned grant's request must equal the one sent.
- **New honest outcome:** a failure after exec but before the first stdin byte is
  attempted returns `ErrSSHBinaryStageNotStaged`. With no header or payload sent, no
  helper (authorized or not) can have staged. This covers challenge failures,
  authorization refusals, consumed/replayed attempts and lost authorization
  acknowledgements. A consumed admission is not replenished.
- After the first stdin byte, everything is `ErrSSHBinaryStageUnknown`, as for `Send`.
  An acknowledgement additionally requires stdout = the exact challenge line followed
  by the matching receipt. The stdout budget grows by the challenge line size only.
- The shared transport now refuses any exec command other than the two fixed
  constants, before the uncertainty boundary.
- `ProbeBinaryStageAuthorizer` is a consumer-side interface in the adapter.
  `services.ProbeBinaryStageAuthorizationService` satisfies it; no new core port.

## What a grant proves, and what it doesn't

The hub's Ed25519 authority signed this exact intent once, after the DB consumed
the attempt, for the one helper process that issued the nonce. That helper's own sshd host keys
include the key the intent pins, and the sender pinned that same key. It does **not**
prove continuous authority (admission is authority at commit), that sshd actually
serves those key files, helper/key-file provenance, remote termination, cleanup
authority or an applied `artifact_directory` resource.

## Verification (local, this working tree)

Executed on Darwin/arm64 with Go 1.26.9, race detector, no DSN:

| Check | Result |
|---|---|
| Provisioning focus: codec + authorized sender (`-run 'ProbeBinaryStageGrantFrame\|ProbeBinaryStageChallengeLine\|ProbeBinaryStageAuthorityKey\|SSHBinaryStageSenderAuthorized'`) | 239 passes (30 top-level), 0 fail, 0 skip. The codec has 100% statement coverage; sender selection repeated `-count=5` without flakes |
| Bootstrap focus: helper unit/effect, host binding and end-to-end (`-run 'AuthorizedProbeBinaryStage\|VerifyProbeBinaryStageGrant\|ProbeBinaryStageAuthorityReadKey\|HostKeyFingerprints\|HostOwnsIntent'`) | 153 passes (29 top-level), 0 fail, 0 skip |
| End-to-end composition (`TestAuthorizedProbeBinaryStageSSH*`) | Real sender → Go SSH server → real helper → real service/signer → real temp parent. Covers success, a replay refused NotStaged, a tampered grant refused Unauthorized with an empty parent, and an untrusted authority refused before admission |
| `-overlay` mutants (no source edited) | Caught: signature verification skipped (e2e tampered grant acknowledged); host binding always true (relay and unit tests fail); 21 of 24 helper mutants. Three survivors are redundant double checks. Sender: request-equality, `notStaged` and context mutants caught; the already-exited guard is caught only statistically, because it is inherently racy |
| `go build ./...`, `go vet ./...`, `gofmt -l internal cmd`, Linux amd64/arm64 CGO0 admin build + vet of the touched packages | Passed |
| Full native race suite (no DSN), `go test -race -count=1 -json -p 4 ./...` | 7,226 passes, 0 failures, 565 skips (560 missing-DSN repository, 5 known optional); 23 passed / 14 no-test packages |
| Independent read-only review | No high/medium findings. Its low findings (admission budget floor, cross-host relay, empty exec command, doc placeholders) were fixed here |

**Not executed:** actual OpenSSH/Linux namespace acceptance of the new verb, the
Linux key loader against a real root-owned `/etc` (only cross-compiled and vetted for
linux/amd64 and arm64), MariaDB (no repository change in B/C), and golangci-lint
(not installed). The Linux protection checks in `probe_binary_stage_authority_linux.go`
are therefore inspected, not exercised.

## Key custody (increment D)

See [key provisioning](KEY_PROVISIONING.md#stage-authority-key-m8-binary-staging-grants).
`auth.ReadProbeSecretKeyFile` is the snapshot key's protected reader, extracted
unchanged so both 32-byte secrets share one implementation. `NewProbeConfigProtectorFromFile`
now calls it. `LoadProbeStageGrantSigner` turns the seed into the existing Ed25519
signer and clears its temporary buffers. No hub startup or config path loads the key
yet; the future installer composition root will.

## Artifact executor (increment E)

`SSHArtifactDirectoryExecutor` implements `ports.ProbeInstallResourceExecutor` for
`artifact_directory` only. It rebuilds the stage intent from the acknowledged
reservation receipt: a running systemd operation, `data_directory` applied, and
`artifact_directory` in flight with its attempt and the derived `/opt` reference. It
borrows a sender, a prepared binary whose pin must equal the identity's, and the
authorizer.

| Send result | Reported outcome |
|---|---|
| Acknowledgement for exactly this intent | `applied` |
| `ErrSSHBinaryStageUnknown` | `unknown` / `outcome_unknown` |
| `ErrSSHBinaryStageNotStaged`, every pre-exec sentinel, other kinds, mismatched receipt or pin | `not_started` |

It never returns an error. The runner records any executor error as unknown, which
would misreport a resource provably never touched.

**Why an acknowledgement may now be `applied`.** Earlier records said not to wire an
ack to `artifact_directory`/`applied` while no current authority existed. That
authority now exists. The DB consumed this exact reserved attempt under
operation-first locking, its deadline and in-flight state. The authority key signed
only after that commit. The helper verified the signature for its own nonce and
refused intents pinned to another host's key. It then created the stage fresh-only
and acknowledged it. `applied` still means "this attempt's directory was staged",
not installed, enrolled or running. A refusal from the helper after the header
(e.g. host binding) is reported unknown, because the sender cannot distinguish it
from a post-stage failure.

**Known cost of a host-binding mismatch.** The helper checks host binding after
the hub has already admitted and signed. A mismatch therefore consumes the
admission, records `artifact_directory` unknown and interrupts the operation. An
honest sender reaches that state only through host misconfiguration, such as sshd
serving keys outside `/etc/ssh/ssh_host_*_key.pub`. Moving the check ahead of
admission would need the header before the challenge. That protocol reorder was
deferred: either way the operation is terminal and needs a new one.

The executor also requires the receipt's whole resource layout to equal the one
derived from the identity, so a malformed receipt never reaches the network.

**Lifecycle evidence (SQLite only).** The test composes the production runner, a
real SQLite journal, the admission store, the authorization service and signer,
the executor, the sender, a Go SSH server and the real helper. It records
`data_directory` applied (explicit test double), `artifact_directory` applied with
its reserved attempt, the staged files and exactly one admission, then
`unit_file` not_started and the operation failed/not_started. A replay while the
journal still shows the attempt in flight is refused by the consumed admission
alone. An untrusted authority yields not_started with zero admissions. A foreign
host key yields unknown/interrupted with one consumed admission and no files. No
MariaDB/InnoDB, real sshd, `/opt`, uid 0 or systemd was exercised.

After D/E, the full native race suite (Darwin/arm64, no DSN) passed: 7,333 passes,
zero failures and the same 565 audited skips.

## Still open

Installing the public key and forced-command policy on hosts; actual OpenSSH/Linux
acceptance of `stage-binary-authorized`; host-authorized executors for
`data_directory`, `unit_file` and `service` (the admission schema admits only
`artifact_directory`); loading the authority key in a hub composition root; the
Docker executor; admin routes; bounded redacted progress; enrollment and
exact config activation.
