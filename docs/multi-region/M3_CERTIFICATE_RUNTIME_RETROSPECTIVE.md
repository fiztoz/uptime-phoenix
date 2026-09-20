# M3 certificate runtime retrospective

Baseline: `18a1e82`; source implementation and fixes are in the commit containing
this record. Scope: source TLS rotation and admission, not completed hub certificate
issuance or the full M3 milestone. Codex owns integration; Antigravity owns no files.

## Summary

The source now serves authenticated durable active certificates and binds runtime
admission to the certificate actually selected by each TLS handshake. Real TLS
verification found a preparation deadline gap; correcting it exposed historical
receipt recovery closing too early. A concurrent cache recovery issue and strict
JSON checks missing from standalone command codecs were also reproduced and fixed.
All original regression cases pass with the final implementation.

## Root cause and mechanism

### A preparation can change the deadline of an existing session

`AcceptCredentialConnection` calculates `ValidUntil` at admission. The first
certificate preparation can occur later, so the established context cannot know
its new overlap deadline. The fixed 12-second receipt grace originally kept that
socket open even when preparation left only two seconds of overlap. The regression
`TestCertificateRuntimePreparationBoundsExistingSession` received the receipt but
still timed out waiting for closure after four seconds. Quiescence already prevented
follow-up mutation, but the socket lifetime violated the contract.

The initial fix capped every successful preparation's grace by its request's
historical deadline. `TestCertificateRuntimeLostActivationReplyAndColdRestart`
then reproduced EOF while recovering an old prepare receipt on the current new
certificate after retirement. Closing immediately can cancel the hub's async
receipt commit, repeating the starvation documented in the hub credential record.

`edgeCommandReconnect` now caps grace for a preparation whose overlap was still
future when execution began and whose prepared identity is different from the
current session. The existing admission context remains authoritative for any
previous deadline. Historical preparation recovery retains bounded receipt grace;
all successful rotations still quiesce further effects until reconnection.
The same fix covers credential preparation. Real socket tests cover both types,
late activation on an already bounded session, and source restart/recovery after
retirement. No deadline or socket close is treated as a durable receipt.

### Recovery waiters repeated an already successful reconciliation

Every handshake observing an empty cache called forced `Reload`. Waiters that
acquired the gate after the first repair invalidated that valid cache and reread
SQLite. Antigravity identified this path. With the fix removed,
`TestCertificateRuntimeConcurrentRecoverySharesReconciliation` observed **32**
durable reads for a burst of 32 callers. With the fix, it observes **one**.
Handshake recovery now double-checks the cache under the gate. Explicit `Reload`
continues to invalidate and authenticate the current durable row, as required for
activation/recovery; failures never select stale bootstrap material.

### Standalone command decoding bypassed strict JSON validation

The certificate codec initially copied the credential/ACK codecs' use of
`objectFields`, which unmarshals into a map. Unlike `DecodeEnvelope`, that helper
alone does not reject duplicate keys or invalid UTF-8. The network path already
performed envelope validation; the separate codec port could accept malformed
input outside that path. All three codecs now call existing `decodeJSONObject`
before typed validation, preserving exact original-byte digests.

A negative control restored the old helpers and all three codec subtests failed
on duplicate fields. The final tests also reject invalid UTF-8. An initial test
incorrectly demanded rejection of unknown top-level optional fields. Protocol V1
explicitly permits those fields, so the test was corrected to assert compatibility.
Per-kind `data` remains closed. No protocol restriction was invented to make a
test pass.

## How it was found and why it slipped through

Storage tests prove atomic selection and receipts but cannot establish which
certificate a real TLS connection used or how a session context ends. The new
fixtures use an actual `http.Server.ServeTLS`, a pinned client, a private SQLite
store, and the production runtime. They hold an old handshake before HTTP admission,
lose the activation response after durable commit, restart from retained files,
and cross the fixed overlap deadline. A first fixture assumed the pinned client
exposed a plain `http.Transport`; it actually wraps one in `pinnedRoundTripper`.
Only that test assumption changed.

The prepared-session tests that already existed opened their socket after
preparation, missing the first socket's newly introduced deadline. The historical
receipt regression demonstrates why both first-application and duplicate-recovery
lifecycles must be tested when changing teardown behavior. Independent negative
controls confirmed that the cache and codec tests detect the removed fixes.

## Antigravity review and coding lessons

Read-only conversation `fdb39cb2-46c7-4e49-90d2-117249ae27c7` completed successfully.
Codex accepted and verified concurrent recovery duplication. Three other findings
were withdrawn after tracing the supported lifecycle:

- Expired cached material must fail closed. All supported active-pointer changes
  pass through the TLS manager and reconcile; an imagined out-of-band DB writer
  is not evidence that an expired cache hides a valid newer certificate.
- The 12-second receipt/reconnect barrier is deliberate. Preparation does not grant
  permission to pipeline activation on the quiescent old session.
- Activation already inherits the admission context's overlap/certificate/credential
  deadline. A local grace timer cannot extend that parent context. The gap was the
  first preparation on a socket admitted before the new deadline existed.

Antigravity acknowledged these distinctions and reported no remaining actionable
finding in this slice. Its review did not execute tests; Codex supplied and ran
all evidence. For future work: identify the actual writer, trace the root context
and durable transaction, test both fresh and duplicate commands, and treat failing
test assumptions as hypotheses rather than changing the protocol to satisfy them.

## Validation

The focused final source suite passed 29 named tests/subtests with race detection
and no failures/skips, using actual TLS, cryptography and private SQLite. The
full final gate, process regression, exact source hashes and artifact hashes are
recorded in [acceptance](M3_CERTIFICATE_RUNTIME_ACCEPTANCE.md) and
[evidence](M3_CERTIFICATE_RUNTIME_EVIDENCE.json). Earlier failed runs remain in the
evidence ledger; they are not presented as passes.

## Follow-up

Codex continues hub certificate issuance, immutable receipt-dependent activation,
candidate pin selection, credential resealing on pin promotion, and dual-engine
plus process recovery tests under [the certificate contract](M3_CERTIFICATE_ROTATION_WORK_CONTRACT.md).
Explicit stream reset must rebind retained certificate encryption metadata safely.
Remaining pressure/flush and the real fifteen-minute partition acceptance stay
under [the M3 completion contract](M3_COMPLETION_WORK_CONTRACT.md). M3 is incomplete.
