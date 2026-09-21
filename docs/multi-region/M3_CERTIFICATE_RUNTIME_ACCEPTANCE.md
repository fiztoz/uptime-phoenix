# M3 certificate source TLS runtime acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Baseline: `18a1e82`. This increment connects protected source certificate storage
to the actual TLS server, session admission and command execution. It advertises
`command.certificate_rotation.v1`. Hub certificate issuance, candidate pin selection
and receipt-driven promotion remain the next increment; M3 is incomplete.

## Implemented effects

`OpenRuntimeIdentityAnchor` preserves file ownership, permissions, manifest,
key-pair, fingerprint and exclusive-lock checks while deferring bootstrap validity
to durable active selection. The normal `OpenRuntimeIdentity` API still rejects
expired bootstrap material. `cmd/probe` authenticates the active protected row
before opening the listener. An expired bootstrap can support recovery only when
the stored active certificate is currently valid. Missing, corrupt, wrong-key or
expired active material never falls back to bootstrap or generates a replacement.

`EdgeTLSManager` invalidates serving before activation and reconciles the committed
selection after both success and ambiguous storage errors. Failed reconciliation
leaves the cache unavailable; a later handshake can perform a bounded authenticated
reload. Concurrent recovery waiters share the repaired cache. Normal handshakes
use immutable validated material and check validity each time. TLS 1.3 is required;
resumption is disabled so each connection records its actual selected certificate.

The HTTP runtime adapter passes that trusted selection into the same SQLite
transaction that rechecks credentials and advances the session generation. An old
handshake delayed past retirement fails without consuming a generation. Certificate
expiry, credential expiry and fixed overlap jointly bound established sessions.
Successful preparation/activation quiesces further effects until receipt confirmation
and reconnect, with a 12-second forced-close limit. A first preparation additionally
bounds a socket admitted before the overlap existed. Historical prepare receipt
recovery on a current identity retains the receipt grace.

The source codecs reuse the existing wire DTOs, preserve exact request digests,
and return only public certificate metadata. Standalone ACK, credential and
certificate decoders now run the common strict JSON validation for duplicates,
UTF-8 and nesting while retaining V1 optional-field compatibility. No dependency,
frontend or Helm change is required; the runtime uses existing edge migration 008.

## Verification

Commands executed: focused race-enabled source/runtime tests, controlled negative
reproductions, final CGO-free build, complete Go race suite with the disposable
MariaDB DSN, golangci-lint, and the compiled two-worker replay/credential harness.
The final result counts and immutable artifact hashes are in
[evidence](M3_CERTIFICATE_RUNTIME_EVIDENCE.json).

Engines and named tests exercised: the source runtime tests use actual generated
certificates, pinned TLS connections, private SQLite, encrypted retained material
and cold reopen. `TestCertificateRuntimeLostActivationReplyAndColdRestart` proves
candidate rejection before activation and original activation/prepare receipt
recovery after cold restart and overlap expiry. Bootstrap files remain byte-identical.
`TestCertificateRuntimeRejectsHandshakeDelayedPastRetirement` holds an old TLS
handshake across activation/expiry and proves no generation advance. Other named
cases cover preparation and activation session deadlines, failed reconciliation,
resumption disabled, a 32-caller recovery burst, expired bootstrap and active
material, and exact-byte/structural codec checks. Admission tests cover irreversible
expiry on clock rollback, forged metadata and competing earliest deadlines.

Passed / failed / skipped: the final full race gate passed in 22 packages with
3,552 named pass events, zero failures and two existing optional skips
(`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`). All 283 MariaDB-named passes ran; 271
match the audited live-engine inventory, with no engine failures or skips. Counts
include parent subtest passes. CGO-free build and lint passed; lint reported zero
issues. All 19 source/test hashes remained unchanged during verification.

The final focused source run passed 29 named cases/subtests, with zero failures or
skips. Controlled removal of the cache fix produced 32 durable reads instead of
one; removing strict codec parsing made all three command-codec duplicate tests
fail. Earlier deadline and historical-receipt regressions also failed before their
respective fixes. See [the retrospective](../postmortems/2026-09-21-m3-integration.md#credentials-and-certificates).

The separate compiled process regression passed all 29 stages: two hub workers,
real edge runtime, enrollment, configuration refresh, offline DOWN/UP and provider
retry/recovery, ordered replay without hub redelivery, credential rotation and
cold restart. It uses a fresh disposable MariaDB schema and the final binaries.
This is regression evidence for the new TLS server, not hub certificate-rotation
acceptance or a fifteen-minute partition run.

Acceptance criteria still unverified: protected hub certificate issuance, exact
receipt-dependent activation, candidate pin selection and promotion with credential
resealing, both-side certificate process recovery after overlap expiry, explicit
stream reset, remaining pressure/bounded flush and the full real fifteen-minute
partition scenario. Subsequent work is accepted in [the later record](M3_HUB_CERTIFICATE_ACCEPTANCE.md).

## Review

Antigravity completed a read-only audit and a corrective teaching response. Codex
reproduced and fixed its concurrent-recovery finding. Three unsupported lifecycle
claims were withdrawn after checking the actual writer, parent context deadline
and receipt/reconnect contract. Antigravity made no file changes or test runs;
its review does not substitute for the executed verification.
