# M3 hub credential rotation acceptance

Baseline: `e23d7e0`. This increment completes the operator-to-source credential
rotation path. Certificate rotation, explicit stream reset and whole-M3 acceptance
remain separate work. No push or deployment is included.

## Implemented behavior

Hub migration 062 adds protected rotation candidates, immutable prepare/activate
identities, per-probe version high-water and the successful prepare result's
credential version. The existing bounded command ledger reserves both requests
atomically; activation stays blocked until the exact prepare result commits.
Failure cancels unsent activation locally without fabricating a source receipt.
Cleanup retains requests referenced by unresolved or retained rotations.

The operator supplies a stable rotation UUID and increasing version. Trusted hub
code generates and protects the token; retries recover the original operation
and deadline. Claiming respects the peer's actual capabilities. The connector
selects the saved candidate after confirmed preparation, including after expiry
when recovering a lost activation result. Only a pinned HTTP 401 before websocket
admission permits the one saved-current fallback. Generic authorization, storage,
network and later-session failures do not permit fallback.

A healthy candidate does not activate the hub credential. The exact durable
activation result promotes it transactionally. Every selection, confirmation,
claim and result is fenced by current database-clock lease/generation authority.
Old duplicate results cannot undo a newer current credential. Neither stream nor
configuration/telemetry identity changes.

After successful rotation the source quiesces the session's further non-health operations and
allows up to 12 seconds for the hub's bounded receipt commit and disconnect. The
existing credential session deadline still applies. The hub closes only after
receipt commit; timeout/close never substitutes for a durable result. The source
forces closure when the hub does not cooperate. See the
[retrospective](M3_HUB_CREDENTIAL_RETROSPECTIVE.md) for the reproduced cancellation
bug that required this handshake ordering.

## Executed effect checks

`TestProbeCredentialRotation` runs on actual SQLite and MariaDB: issuance/retry,
protected candidate/request recovery, prepare gating, no promotion on candidate
health, both failure paths, stale/expired authority, final-write rollback,
concurrent issue, capability separation, pending dependency retention, late
activation receipt after deadline, original result equality and downgrade guards.
The existing command and registry migration suites are also run twice against the
same disposable schema to expose changes left behind by a rehearsal.

`TestProbeConnectorCredential*` covers bounded fallback, rejected selection,
lease release and no reuse of the initial-enrollment activation callback.
`TestHubCredentialRuntimeLostResultAcrossBothStoresRestart` connects two durable
SQLite stores over pinned TLS, loses a result after source activation commits,
restarts both stores and recovers the exact protected request and original result.
`TestCredentialRuntimeUnconfirmedResultQuiescesAndCloses` verifies the forced-close
bound and rejection of further effects before reauthentication. Existing source
expiry and ACK runtime tests remain in the gate.

The real process harness runs two hub workers, the operator CLI and the edge with
a fresh MariaDB hub database. It queues rotation behind a network partition,
restarts both sides, confirms one source receipt per command, then cold-restarts
again using the promoted credential and proves continued telemetry progress.
The final run completed 29 stages. Its first run failed at source preparation
receipt starvation, as preserved in the retrospective. Exact source, binary and
log hashes are recorded in [the evidence file](M3_HUB_CREDENTIAL_EVIDENCE.json).

Commands executed: focused race-enabled DB and TLS/service tests; compiled process
harness with `--verify-replay --verify-credential-rotation`; CGO-free
`go build ./...`; `go test -race -count=1 -timeout=20m -json ./...` with
`TEST_MARIADB_DSN` set; `golangci-lint run`. All final commands exited zero.
Engines and named tests exercised: real SQLite source/hub and disposable MariaDB
hub; explicit named cases above.
Passed / failed / skipped: 22 tested packages, 3,477 named test passes, 283
MariaDB-named passes, zero failures and zero MariaDB skips. Two existing optional
tests skipped (`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`). Lint reported zero issues. All 32 changed
source/test/migration/harness hashes remained unchanged through the final gate.
Acceptance criteria still unverified: certificate rotation, explicit stream reset,
remaining pressure/shutdown flushing and the real fifteen-minute failure/recovery,
restart/replay/current-state/offline-ACK scenario. M3 is incomplete.
