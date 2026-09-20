# M3 certificate rotation work contract

Baseline: `a525ca0`, 2026-09-21. The complete M3 goal remains active.
Codex owns every source, test, migration and documentation file. Antigravity
owns no files; its design review is advisory. No push or deployment.

## Durable source authority

- Reuse the closed certificate.prepare/activate DTOs. Stable command and rotation
  UUIDs, exact request digest, source session fence and increasing version remain
  mandatory. Version 1 is the initialized bootstrap certificate.
- Generate a P-256 key and self-signed server certificate locally. Protect PEM
  with the existing local key and a distinct AES-GCM purpose binding hub, probe,
  stream, rotation, version, creation, fingerprint and certificate validity.
  Neither private keys nor protected PEM appear in command results or hub data.
- Migration 008 adds the protected rotation journal, persistent active/high-water
  versions and successful prepare result metadata. Preparation plus original
  receipt commit together. Activation pointer, activation time and receipt commit
  together. No certificate or manifest file must be overwritten to activate.
- Identity files and edge_identity.fingerprint remain an immutable bootstrap
  pairing check. Active certificate metadata is selected from the certificate
  journal; it is never confused with that anchor or serialized from a domain type.
  Later explicit stream reset must authenticate and rebind retained active material
  to the new stream scope atomically; changing only a stream column would break AEAD.
- The fixed overlap ends ten minutes after preparation command creation. Source
  activation must precede that deadline; retries do not extend it. Observed closure
  persists across clock rollback. Preparation does not switch the served certificate.
  Duplicate success returns original metadata/time even after expiry and restart.
- Refuse overlapping credential/certificate operations. Retain version high-water
  after cleanup. Never prune the active certificate or an unresolved dependency.
  Bound retained rotations to 1,024, each protected PEM to 16 KiB plus AEAD overhead,
  and use the existing shared bounded command receipt ledger.

## Runtime and hub integration

- Separate structural bootstrap opening from verified active TLS loading. Only
  the runtime recovery path may defer bootstrap expiry; it must authenticate and
  validate a currently valid active certificate before listening. An unrotated
  identity with an expired bootstrap certificate still fails. Missing/corrupt
  active material never falls back to bootstrap or generates a replacement.
- SQLite owns activation. A TLS cache may publish only verified committed material;
  startup reconstructs it. Invalidate serving during an activation/load failure;
  do not leave an old cache authoritative after a successful DB commit.
- Bind each server TLS handshake to the selected certificate identity and recheck
  eligibility at session admission in the same transaction as credential/fence
  admission. Previously started handshakes cannot escape retirement. Bound sessions
  by certificate expiry and any overlap deadline. Keep the existing receipt-before-
  reconnect ordering and 12-second quiescence bound.
- Hub saves preparation receipt/pin before issuing activation. Candidate TLS pin
  and credential AEAD metadata are distinct inputs until promotion. Current
  credential ciphertext must be resealed under the new fingerprint in the same
  fenced transaction as receipt-driven hub promotion.
- After confirmed preparation, use the candidate pin for lost-activation recovery,
  including after overlap expiry. Old-pin fallback is bounded by the original
  deadline. After expiry, an uncertain activation that never reached the source
  can require explicit verified operator recovery; do not silently extend dual trust.
  A completed candidate handshake alone is not a fabricated activation receipt.
- Capability remains unadvertised until actual source execution/TLS recovery is
  wired. Operator issuance remains unavailable until the hub path is verified.

## Required effect tests

Real SQLite: original prepare/activation receipts across reopened stores; no
plaintext PEM in DB/WAL; tampered ciphertext/metadata/wrong key rejection; exact
version/pin matching; high-water/non-overlap; explicit expiry/clock rollback;
late write rollback for journal, pointer and receipt; capacity/dependency retention;
paired migration preservation and downgrade refusal.

Pinned TLS and processes: no candidate before activation; new candidate after
commit; no stale cached serving/admission after retirement; lost activation reply
and both sides cold restart after deadline; expired bootstrap with a valid active
certificate; corrupt/expired active material fails closed; state/config/stream/
telemetry continuity. Hub persistence runs on SQLite and MariaDB, with stale lease,
late-write rollback, immutable retry, strict result details and capability routing.
Full build/race/lint gate and exact named engine counts precede acceptance/commit.

## Antigravity review disposition

Conversation `95a7fbec-80fe-4f08-88d4-b5046ce8b1cc` completed successfully using only
supplied source and design. Accept the distinction between bootstrap and active
identity, explicit post-deadline candidate recovery, separate credential AAD and
network pin, and avoiding unbounded DB I/O on each TLS handshake. The proposed
bootstrap recovery path and AAD separation were already in the supplied design;
they are implementation obligations, not newly observed implementation bugs.

Do not adopt invented line numbers or a sub-10ms handshake target. No benchmark
was run. A post-commit atomic-pointer assignment alone is insufficient: failure
between commit and cache refresh needs fail-closed serving and cold-start recovery.
No added duplicate active-fingerprint column is required when the selected journal
row already supplies it. Test actual effects and bounds rather than trusting a
reviewer's prose or setting an arbitrary performance gate.

The bounded storage audit and corrective teaching follow-up in the same
conversation also succeeded. The claim that Go always leaves `Certificate.Leaf`
nil was false for the required Go 1.26.6 default. A direct compiler reproduction
confirmed the narrower compatibility-setting panic, now fixed by explicit parsing.
The proposed immediate overlap closure was rejected because it contradicts this
contract. See [the verified retrospective](M3_CERTIFICATE_STORAGE_RETROSPECTIVE.md).

## Runtime implementation notes

The next increment must bind the certificate actually selected for each TLS
handshake to its later HTTP/WebSocket admission. Go's `ClientHelloInfo.Context`
inherits `HandshakeContext`, and net/http performs that handshake using its
connection context. Use a private per-connection binding established through
`http.Server.ConnContext`, populated by the TLS selector, and read only after
the handshake. Do not trust a request header to identify the served certificate.

Configure the dynamic selector without a static Certificates fallback. Disable
TLS session tickets/resumption for this narrow probe listener so every new
connection has an observed certificate selection; verify that path with a real
resumption-capable client. Recheck that binding against durable current/previous
eligibility in the same source transaction that claims the session generation.
Its expiry bounds the session together with the credential deadline. Test an
old handshake delayed across activation and retirement, not just immediate
post-rotation dials.

This is a verified API/design direction, not an implemented runtime claim. The
current serving path still uses the bootstrap certificate. Codex retains all
implementation and verification ownership.


## Hub continuation after source TLS wiring

The source runtime increment is described in
[M3_CERTIFICATE_RUNTIME_ACCEPTANCE.md](M3_CERTIFICATE_RUNTIME_ACCEPTANCE.md).
The following is an implementation handoff, not acceptance of unbuilt hub behavior.
Codex retains file ownership; Antigravity may review supplied changes read-only.

- Trace `ProbeConnectorService.connectOnce`: it currently uses the same
  `ProbeCredentialMetadata.Fingerprint` both to authenticate retained credential
  ciphertext and to select the network TLS pin through `ProbeSessionInput`.
  Introduce a separate explicit trusted dial pin for pending certificate recovery.
  Do not mutate credential metadata to a candidate pin before decryption, and do
  not infer source activation from a successful candidate handshake.
- `ProbeCommandStore.CompleteCommand` already serializes current lease authority,
  operation transition and source receipt, then rechecks the lease deadline before
  commit. Extend this path for certificate preparation and activation. Persist the
  source's exact prepared version/pin/expiry before activation becomes dispatchable.
  Its immutable activation body cannot be built before the prepared pin exists;
  reserve a stable activation command ID at issuance and account for its pending
  storage capacity. No dispatchable placeholder payload may be recorded as success.
- Add paired hub migrations for certificate operation state, active/high-water
  versions and receipt metadata. Recheck the current migration maximum before
  reserving the number. Preserve pending command dependencies and refuse unsafe
  downgrade. Retain the original request/receipt after expiry for recovery.
- During activation confirmation, decrypt the current runtime credential with its
  old authenticated metadata, reseal the same token under the new fingerprint,
  and atomically promote connection/registration certificate metadata with the
  source receipt. Credential and certificate rotations exclude one another on
  both engines; a saved earlier credential operation cannot undo a newer pin.
- Extend capability-aware claims and strict per-kind outcome checks. Certificate
  details are valid only for successful certificate preparation, never ACK,
  credential or activation results. Successful certificate results use the existing
  hub-commit-before-close lifecycle. Unsupported peers must not receive these
  commands or starve other supported pending work.
- Candidate pin selection and confirmation must share the current connector fence.
  Old-pin fallback is allowed only before the original overlap deadline and before
  any source session was admitted. Use a typed pre-WebSocket TLS mismatch outcome,
  not string matching or catch-all fallback. Persist observed overlap retirement
  so a backward clock cannot reopen old trust. After expiry, candidate-only
  recovery can require explicit verified operator action if activation never ran.

Verify actual SQLite and MariaDB effects, stale leases, late-write rollback,
immutable retry, wrong-pin/wrong-key rejection, operation exclusion and retained
high-water. Then run source and hub cold restart with an activation reply lost
before the hub receipt commit, including recovery after overlap expiry. Existing
compiled credential/replay acceptance is only a regression baseline for this step.


## Hub implementation record

The journal, protected dispatch, candidate/current TLS selection, receipt-driven
pin/token promotion and operator CLI are now implemented. Hub connection metadata
is canonical in `probe_connections`; no duplicate pin column exists in the probe
registration table. The reserved activation row has no payload/digest and cannot
dispatch until preparation commits. Both source and hub exclude overlapping
credential/certificate operations. Consult
[hub certificate acceptance](M3_HUB_CERTIFICATE_ACCEPTANCE.md) for the authoritative
verification status; historical implementation directions above are not current
missing-feature claims. Explicit stream reset and the full M3 partition remain.
