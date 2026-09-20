# M3 credential rotation work contract

Baseline: `2a4e198`. This work completes the existing credential prepare/activate
protocol; it does not complete certificate rotation or the whole M3 milestone.
Codex owns all edits and verification. Antigravity has read-only review ownership
and must not edit project rules, source, tests, or reports.

## Source contract

- Prepare binds a stable rotation ID, a strictly increasing credential version,
  the token digest and an absolute overlap deadline no later than ten minutes
  after command creation. Only digests cross the source persistence port.
- Activation is a separate command for that exact rotation and version. It must
  commit before the fixed overlap deadline. It atomically promotes the digest,
  preserves the previous digest through the original deadline and records its
  receipt. Retry never extends the window or changes the stream/configuration.
- The prepared token is usable during the overlap. After activation the new token
  remains usable; the previous one expires at the fixed deadline. If activation
  never happens, the original active token remains usable after the failed window.
- At most one rotation may occupy an unexpired overlap. Version high-water survives
  cleanup. Rotation identities and command receipts remain bounded and retained
  through the existing one-year receipt horizon; capacity failure rolls back.
- Current authenticated generation fences every command, including duplicates.
  Exact request bytes and decoded effect values bind receipts. Effect and receipt
  commit together. A duplicate recovers the original result after request expiry.
- Socket admission rechecks the credential and persists its generation in one
  transaction. Before activation both admitted credentials carry its deadline;
  after activation only the previous credential carries that deadline. The newly
  active credential remains valid beyond the overlap, as required for recovery.
  Successful prepare/activate (including receipt recovery) terminates that session
  after writing its result. This forces pre-rotation sessions to reauthenticate;
  admission/command generation fencing closes the concurrent-upgrade race.
- The edge command capability is advertised only with persistence, authentication,
  admission deadlines and runtime dispatch wired. Hub ACK issuance stays closed to
  other kinds until hub rotation persistence/connector recovery is implemented.

## Hub continuation contract

Persist the protected candidate and exact prepare request before dispatch. A
confirmed prepare releases a separate durable activation request. Recover a lost
activation receipt using the candidate credential and the pinned TLS identity.
Prepared-token authentication alone is not activation confirmation. Any old-token
fallback must remain bounded by the stored rotation state; never disable pin or
credential validation. Operator output must contain no plaintext token.

## Required evidence

Real source SQLite tests cover rollback at each commit boundary, restart, duplicate
receipts, stale sessions, immutable rotation conflicts, overlap expiry, admission
races and bounded storage. Runtime tests cover real TLS authentication and deadline
closure, successful prepare/activate and lost replies. Hub integration requires
both SQLite and MariaDB, protected-at-rest inspection, current lease fencing and
candidate recovery. Full build/race/lint gates and coherent commits are required.
Source acceptance alone is not end-to-end rotation acceptance.

## Next hub integration seams

These are implementation requirements, not completed work:

1. `ProbeCommandStore` currently creates, claims and completes ACK only.
   Introduce protected rotation issuance under the same installation/registration
   transaction authority. Persist candidate credential metadata/ciphertext and
   immutable prepare/activate identities before any send. Use a new hub migration
   after checking both engine directories; the source migration number is separate.
2. A confirmed successful prepare must atomically make its matching activation
   request dispatchable. The prepare result's version must equal the saved
   candidate version. Failure must remain an explicit failed rotation, without
   fabricated activation or a prematurely promoted connection credential.
3. Claim selection must honor actual peer capabilities per command kind. Current
   `HubTransport` starts the sender only for `command.alert_ack.v1`, and the
   repository query fixes `kind = alert.ack`. Extending one gate alone is unsafe:
   unsupported commands must neither be sent nor starve compatible pending ACKs.
4. `commandOutcome` and hub storage currently reject prepare details. Extend their
   closed validation together with durable result columns; ACKs still require
   absent details, and credential prepares require the exact expected version.
   Duplicate-result equality must include the new result metadata.
5. `ProbeConnectorService.connectOnce` currently reads exactly one protected
   connection. Its health callback calls `ActivateConnection`, which confirms
   original enrollment only. A prepared candidate may authenticate before source
   activation, so this callback cannot be reused as rotation activation proof.
6. Select the saved candidate after confirmed preparation and across an uncertain
   activation reply. Source activation success alone promotes the saved current
   connection. The candidate retains the same endpoint, certificate pin, hub,
   stream and enrollment identity, with its new credential version bound by AEAD.
7. `HubTransport.dial` already returns `domain.ErrUnauthorized` specifically for
   HTTP 401. Any old-credential fallback must be limited to that rejection before
   establishment, remain tied to the saved pending rotation, and keep all TLS pin
   checks. A generic connection/config/storage error must not trigger fallback.
   Candidate and current are the only bounded alternatives. Test the exhausted
   unactivated window separately from activation-applied/result-lost recovery.
8. Result promotion, candidate selection and reconnect callbacks must keep current
   DB lease/generation authority. Retrying an older rotation result must never
   move the saved current credential backward after a newer rotation.
9. Add a metadata-only operator issue/status path, generate secrets in trusted hub
   code, and persist protected bytes before dispatch. Use real SQLite and MariaDB
   for rollback/restart/competing-owner tests and the compiled process harness for
   the complete supported rotation workflow before declaring it operational.

## Receipt/reconnect integration correction

The real TLS and process tests reproduced receipt starvation: immediate source
close after the result write cancelled the hub's independent callback before its
transaction could commit. Successful source rotation now quiesces further
non-health work and waits for the hub to close after durable confirmation, with
a 12-second forced-close bound and the original credential deadline still in
force. The hub closes only after its bounded 10-second receipt commit succeeds.
Neither close nor timeout is an application receipt; lost results still retry
the exact persisted bytes. Health remains independently serviced and all workers
are joined. No new wire frame is introduced.
