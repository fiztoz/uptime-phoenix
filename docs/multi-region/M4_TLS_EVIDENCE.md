# M4 remote TLS evidence

This slice carries certificate metadata from remote checks through source
persistence, current-state transfer and historical replay. It advances M4's
auxiliary evidence requirement; certificate notifications, capacity state and
the rest of M4 remain open.

## Behavior and ownership

The edge recorder reads only `tls_not_after`, `tls_days_remaining` and
`tls_issuer` from a checker result. Exact expiry is required; rounded remaining
days never manufacture a certificate identity. Missing or malformed expiry means
no TLS evidence for that observation. Issuer text is limited to the protocol's
256-byte budget without splitting UTF-8. Raw certificates, keys and unrelated
checker metadata never enter this payload.

TLS accompanies the existing availability observation, including a failed HTTP
status after a successful TLS handshake. It does not change availability,
allocate an incident, reserve a notification or modify certificate alert cursors.
The observation and its exact current-event bytes commit together in edge SQLite.
No edge migration is needed: both existing byte stores already retain the V1
`tls` object. Restart and acknowledged-history pruning preserve current evidence.

The hub maps explicit wire DTOs into pure domain evidence. Migration
`066_probe_tls_evidence` adds nullable `tls_json` columns to `probe_observations`
and `monitor_probe_state` on SQLite and MariaDB. Explicit storage DTOs preserve
the certificate's exact expiry, including fractional seconds, independently of
the microsecond SQL identity used for observations. Existing rows retain null TLS.

Accepted current-state writes refresh the existing `tls_info` view for the exact
monitor, probe and assignment generation in the same fenced transaction. The
current source sequence determines ordering even when the clock moves backward.
Older replay, retired assignment history and stale sessions cannot replace this
view. Historical replay retains its own TLS sample without manufacturing provider
work on the hub. Duplicate receipts preserve the original writes.

A newer observation with explicit null clears its current TLS view. Complete
snapshot omission also clears that assignment's TLS projection while retaining
historical samples. Repeated snapshots cannot alter the TLS fields of the same
source observation. Failed cursor or snapshot-receipt writes roll back TLS,
availability, history and receipt changes together.

## Operator limits and upgrade order

No additional configuration is needed to retain metadata supplied by a remote
HTTPS checker. Monitor certificate notification settings remain explicitly
unsupported by remote configuration publication/activation. This slice does not
enable certificate threshold paging, acknowledgements or notification cursors.
Regional certificate selection in the administrative UI/API remains M5; existing
local compatibility readers still select only the current local assignment.

Use the upgraded hub and schema before starting upgraded probes. Older hubs
reject observations and current snapshots containing TLS evidence; this slice
does not negotiate a fallback that discards certificate data. Old probes remain
able to send null TLS to the upgraded hub. Mixed-version deployment rehearsal
remains part of the open operational compatibility work.

Run schema changes with application writers stopped. Migration 066's down path
refuses to drop either new column while it contains TLS evidence. Do not delete
history merely to force a downgrade; preserve the database and choose an upgrade
or restoration procedure that retains the evidence.

## Verification coverage

Authored source coverage exercises a real local HTTPS target with the production
HTTP checker and recorder, actual edge SQLite, close/reopen, replay bytes, ACK
pruning and the retained current observation. Service and protocol tests verify
exact expiry, invalid/missing expiry, bounded UTF-8 issuer, explicit wire fields,
and mapping into replay and current-state DTOs.

`TestProbeTLSEvidenceAcceptance` runs the following cases on both hub engines:

- `HistoryCurrentAndNull`: exact fractional expiry, duplicate receipt, backward
  clock, certificate replacement, null clearing and no hub provider work.
- `SnapshotAheadOfHistoryAndImmutableTLS`: current evidence before replay,
  same-sequence conflict, omission, later recovery and explicit null.
- `ReplayAndSnapshotRollback`: faults at the final cursor/receipt boundary leave
  the prior certificate and all other persisted effects intact.
- `StaleAuthorityAndHistoricalAssignment`: old session rejection and retained
  history without replacing current certificate evidence.
- `ConcurrentSnapshotAndReplay`: simultaneous writers preserve the newer source
  sequence and its certificate on both SQLite and InnoDB.
- `MigrationRoundTripAndEvidenceGuard`: populated null rows survive down/up;
  stored TLS prevents destructive downgrade.
- `InvalidTLSRejectedBeforePersistence`: bounded core validation also applies to
  callers that bypass the wire decoder.

The compiled process harness adds `--verify-tls`, requiring `--verify-replay`.
It makes the main HTTP target a local HTTPS fixture and compares source TLS with
hub history, current state and `tls_info` across offline failure/recovery, process
restarts, replay and ACK pruning. It retains the existing exact incident/delivery
and zero-hub-send assertions and can run with `--verify-docker`.

Executed results are recorded below and in the accompanying
[hashed evidence](M4_TLS_EVIDENCE.json).


## Executed evidence

Commands executed:

- `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...` — pass.
- `GOTOOLCHAIN=go1.26.6 go test -json -race -count=1 -timeout=2400s -p 4 ./...`
  with the disposable `phoenix_m4_tls_ci` MariaDB DSN, including
  `parseTime=true&loc=UTC&multiStatements=true` — pass in 796.012 seconds.
- `GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run --timeout=5m` —
  zero issues.
- Fresh CGO-free app/probe/admin binaries through
  `scripts/probe_runtime_smoke.py --verify-tls --verify-docker --verify-replay`
  with a fresh disposable `_smoke` schema — pass in 72.029 seconds.
- `gofmt -l internal`, `git diff --check`, production core framework/driver-import
  inspection and changed-document link checks — clean.

Engines and named tests exercised:

- Edge SQLite, hub SQLite and MariaDB 11.8.9, using the existing local disposable
  container `phoenix-m04-checker-coverage` and new test schemas.
- All source/protocol tests and every SQLite/MariaDB acceptance case named above
  executed. The evidence generator requires all seven exact MariaDB leaf cases;
  it rejects missing coverage or a MariaDB skip.
- Compiled production processes exercised the ordinary scheduler, recorder,
  source SQLite, pinned management transport, current-state receiver, replay and
  hub MariaDB projection with two workers and actual process restarts.

Passed / failed / skipped:

- Final race gate: 3,791 named passes across 22 tested packages,
  zero failures, 324 MariaDB-named passes and zero MariaDB skips.
- Two existing optional tests skipped: `TestDatabaseChecker_Check_MongoDB_RealServer`
  and `TestTelegramSender_Send_DownSeverity`.
- 29 process stages passed; 41 retained events replayed, including
  12 exact TLS samples. The original availability incident and two delivery
  outcomes were preserved with zero hub provider-send intents.
- Early focused runs exposed fixture mistakes in checker invocation, enrolled
  hub identity and replay byte bounds. Those were corrected before the final
  full gate. No failing or skipped MariaDB case is counted as accepted.
- The 24 changed Go/SQL/Python files remained identical during the final
  full gate and compiled process run; hashes and artifact paths are recorded in
  [the evidence manifest](M4_TLS_EVIDENCE.json).

Acceptance criteria still unverified:

- Remote certificate threshold paging and capacity promotion/lifecycle remain
  unimplemented and are not established by this evidence.
- Mixed-version deployment rehearsal, remaining M4 escalation/recovery/backup/
  deployment compatibility, and M5 regional administrative/browser views.
- The extended M3 fifteen-minute partition, watchdog, ACK, rotations and stream
  reset process scenarios were not rerun for this slice; their existing Go
  regression tests were included in the full suite.

This is local engineering acceptance. Nothing was pushed, deployed or migrated
in a production database.
