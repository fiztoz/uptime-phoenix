# M0–M4 review fixes

Verified 2026-09-27 against the working tree based on `5873a79`.

## Corrected behavior

1. **Escalation does not cancel earlier DOWN deliveries.** Advancing a firing
   availability incident's ladder increments its transition version. The edge's
   final database authorization now permits older DOWN intents from that same
   incident while it remains firing and unacknowledged. Recovery and ACK still
   suppress them. Certificate, capacity and recovery intents retain exact-version
   checks.
2. **History clearing follows stream identity.** A reset restarts sequence numbers
   without changing assignment generation. Clear-history records the current
   stream alongside its maximum observation sequence; the sequence fence applies
   only to that stream. The observation-time fence still covers delayed pre-clear
   evidence. Repeated clears preserve the largest timestamp, and the largest
   sequence within the same stream, including after a backward clock change.
3. **Restore stays paused until assignments and links are ready.** Imports with
   explicit assignment sets create inactive monitors, restore the graph, then
   activate those originally active. Removing the Bun model's `default:true` tag
   ensures an explicit false reaches SQL; otherwise SQLite omitted the column and
   the schema default activated the row. Failed assignment cleanup reports the
   remaining inactive monitor's id; failed activation is also reported.
4. **Remote evidence can resolve public incidents.** Both replay and current-state
   ingestion invoke the existing status-page recovery service after commit. They
   first read current overall policy health; only an explicit fresh UP can resolve
   a page. DOWN, PENDING, UNKNOWN, maintenance, absent status and failed reads do
   not resolve it. Historical event status alone cannot trigger recovery, and
   this hook does not call the direct monitor notification dispatcher.

## Migration 070

Both hub engines add `history_clear_watermarks.through_stream_id`. For legacy
watermarks, the migration selects the latest retained stream created at or before
the clear. It does not bind an old sequence fence to a replacement created after
the clear. If no such stream exists, the observation-time fence remains in force.

The down migration refuses while any watermark exists: discarding the stream
scope would once again suppress fresh replacement-stream observations. Use a
compatible forward migration or a compatible backup for operational rollback;
do not delete watermarks merely to bypass this guard.

## Verification evidence

Commands executed (all final runs exited successfully; disposable MariaDB DSN
was supplied through `TEST_MARIADB_DSN`):

```sh
rtk proxy env GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./...
rtk proxy env GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
rtk proxy env GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/govulncheck ./...
rtk proxy git diff --check
```

Engines and named tests exercised:

| Test | Executed coverage |
|---|---|
| `TestEdgeEscalationPreservesEarlierDelivery` | Edge SQLite: direct DOWN and first rung remain authorized after two advances; recovery revokes both |
| `TestHistoryClearSurvivesStreamReset` | SQLite and MariaDB: new fence and populated legacy upgrade, reset before upgrade, downgrade refusal, repeated clears, backward clock and fresh replay |
| `TestBackupRestoreSchedulerAdmission` | SQLite and MariaDB: production import, committed local placeholder, scheduler admission reads, final remote assignment and activation |
| `TestBackupRestoreKeepsMonitorInactiveUntilAssignmentsCommit` | Service fake: success, assignment failure, cleanup failure and activation failure |
| `TestRegionalRecoveryResolvesPublicIncident` | SQLite and MariaDB: production replay and snapshot ingestion keep a page incident open on DOWN and resolve it on UP |
| `TestRegionalRecoveryRequiresFreshOverallUp` | Service fake: UP, DOWN, UNKNOWN, PENDING, maintenance, missing assignment, failed read and duplicate ids |

Passed / failed / skipped: the final race run passed all 22 packages containing
tests, with 4,007 passing test/subtest events and no failures. Every named MariaDB
case above executed and passed. Two unrelated tests skipped:
`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`; 13 additional packages had no tests.
The CGO-free build passed; golangci-lint reported zero issues. Govulncheck found
zero vulnerabilities affecting the code or imported packages; it reported three
vulnerabilities in required modules whose affected code is not called.

Acceptance criteria still unverified: no new compiled-process restart smoke test
or browser E2E run was performed for these fixes. The integration tests use real
storage and production service entry points; they do not establish deployment or
browser acceptance. Frontend, wire protocol shapes and dependencies are unchanged.

The [focused gate](../TESTING.md#m0m4-review-regressions) checks required engine
results explicitly so missing MariaDB configuration cannot count as a pass.
