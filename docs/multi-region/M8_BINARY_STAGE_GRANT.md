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
| B — host verification | working tree | `phoenix-probe-admin stage-binary-authorized`: trusted key, fresh nonce challenge, grant check before any payload byte or filesystem effect |
| C — authorized delivery | working tree | `SSHBinaryStageSender.SendAuthorized`: challenge → one `Authorize` call → header + grant frame + payload |

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

**Operator policy:** force the staging key to the authorized verb, otherwise the
legacy unauthorized verb remains reachable by any key holder:

```text
restrict,command="/usr/local/bin/phoenix-probe-admin stage-binary-authorized" ssh-ed25519 <operator-managed-public-key>
```

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

## Still open

Persistent authority-key custody and a way to export the public key file; installing
the key and forced-command policy; actual OpenSSH/Linux acceptance of
`stage-binary-authorized`; wiring `SendAuthorized` into a concrete executor behind the
install runner; the Docker/systemd executor; bounded redacted progress; enrollment and
exact config activation.
