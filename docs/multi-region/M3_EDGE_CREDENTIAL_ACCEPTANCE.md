# M3 source credential rotation acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Baseline: `2a4e198`. This increment implements the source half of credential
rotation. Hub rotation issuance, candidate selection and automatic recovery remain
open; M3 is incomplete. No push or deployment is part of this increment.

## Implemented behavior

Edge migration 007 adds bounded digest-only preparations, previous-credential
overlap metadata, irreversible observed retirement and a retained version
high-water. Command preparation/activation share the existing immutable source
receipt transaction. Exact bytes and decoded effects bind command identity;
current session authority is checked even when recovering a result after expiry.
Prepare results include the version and never a token. Source SQLite/WAL files
were inspected by the test for absence of the known plaintext fixture tokens.

Authentication returns only current or eligible overlapping identities. Socket
admission rechecks that eligibility while committing a newer generation. The
runtime honors the returned deadline and forces reauthentication after writing a
successful prepare/activate result, including duplicate recovery. A lost
activation response can be recovered with the new credential after source restart.
An unactivated expired candidate leaves the original active credential usable.

The probe composition root advertises `command.credential_rotation.v1` with these
paths wired. Hub command issuance remains ACK-only and rejects unexpected rotation
details. The wire/source capability does not supply an operator workflow.

## Executed effect tests

- `TestEdgeCredentialRotationRestartAndLostActivationReceipt`: immutable results
  after source reopen and expiry, current-generation fencing, no plaintext source
  token, unchanged stream/configuration/sequence and exactly two command receipts.
- `TestEdgeCredentialRotationAtomicFailures`: six injected SQL failure points
  spanning preparation, version high-water, current credential, activation and
  receipt writes; rollback leaves no partial effect and retry succeeds.
- `TestEdgeCredentialRotationImmutableIdentityAndVersion`,
  `TestEdgeCredentialRotationBoundsAndHighWater` and
  `TestEdgeCredentialRotationDuplicateConcurrency`: immutable identity, fixed
  deadline, increasing versions, one overlap, bounded storage and one effect.
- `TestEdgeCredentialRotationAdmissionFencesPrepare` and
  `TestEdgeCredentialRotationCachedExpiredAdmissionPersistsRetirement`: real
  competing source transactions and stale HTTP-authentication admission rejection.
- `TestEdgeCredentialRotationExpiryDoesNotBrickOrRevive` and
  `TestEdgeCredentialRotationExpiredCommandCannotReviveOverlap`: exact deadline,
  unactivated recovery and irreversible retirement across a backward clock step.
- `TestEdgeCredentialRotationDowngradeGuard` and
  `TestEdgeCredentialRotationMigrationPreservesAcknowledgement`: refuse retained
  rotation loss, permit empty downgrade, preserve existing ACK effect/receipt
  across down/up schema migration.
- `TestCredentialRuntimeLostActivationReplyRecoversWithNewIdentity`: actual
  pinned-TLS HTTP authentication, runtime commands, SQLite commit, dropped result,
  source restart, candidate reconnect and original receipt recovery.
- `TestCredentialRuntimePreparedSessionExpiresAndOriginalRecovers` and
  `TestCredentialRuntimePreviousSessionExpiresAfterActivation`: real established
  sockets close at the fixed deadline; expired tokens fail and the surviving
  credential reconnects.

The transport tests use a small client harness and a source-result fault wrapper;
they do not use a durable hub rotation issuer. Existing ACK transport recovery was
rerun alongside them. The independently reproduced clock defect and Antigravity
review disposition are recorded in [the retrospective](../postmortems/2026-09-21-m3-integration.md#credentials-and-certificates).

## Full gate evidence

Final results are recorded in [the evidence manifest](M3_EDGE_CREDENTIAL_EVIDENCE.json).
Only source hashes stable throughout the gate may be accepted. The first full
run exposed [the older migration-rehearsal defect](../postmortems/2026-09-21-m3-integration.md#migrations-and-fixtures).
Its corrective twice-run SQLite/MariaDB matrix passed; the final gate uses a fresh
disposable schema and includes the corrected rehearsal.

Commands executed: `CGO_ENABLED=0 go build ./...`,
`go test -race -count=1 -timeout=20m -json ./...` with `TEST_MARIADB_DSN` set to
fresh disposable `phoenix_m3_credential_ci`, and `golangci-lint run`, on Go 1.26.6.
Whitespace checks, core import-boundary inspection and changed-document link
checks also passed.

Engines and named tests exercised: source effects use real SQLite; source transport
uses real pinned TLS. The full existing hub suite exercises SQLite and the
disposable MariaDB database; that is regression coverage, not new hub rotation
acceptance.

Passed / failed / skipped: 22 tested packages, 3,428 named pass events, zero
failures; all 264 MariaDB-named pass events executed with zero MariaDB skips
(37 capitalized `MariaDB` names and 227 lowercase `mariadb` names). The existing
MongoDB real-server and Telegram DOWN-severity cases were skipped; 13 packages
contain no tests. Build and lint exited zero; lint reported zero issues. All 17
changed Go/SQL hashes remained unchanged throughout this final gate. The migration
rehearsal/ACK matrix separately passed twice: 126 named passes, including 60 MariaDB
pass events, with no failures or skips.

Acceptance criteria still unverified: protected hub rotation issuance and
automatic candidate recovery; certificate prepare/activate; explicit stream reset;
remaining pressure/shutdown flushing; the full real fifteen-minute partition with
target failure/recovery, restart, replay, fresh state ahead of backlog and pending
ACK exactly once. No new frontend, Helm, deployment or operator-installation
migration acceptance is claimed.
