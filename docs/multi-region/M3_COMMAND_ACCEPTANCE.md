# M3 durable regional ACK command acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

This increment connects the source ACK transaction from `7fee89e` to protected
hub persistence, the operator CLI, the authenticated transport and authorized
incident replay. It implements `alert.ack`; it does not complete rotations,
stream reset, shutdown flushing or the full fifteen-minute M3 scenario.

## Implemented behavior

Hub migration 061 retains the exact encrypted command payload, immutable
hub/probe/stream/incident/generation identity, digest, retry metadata and source
outcome. The AEAD purpose is distinct from config and runtime-credential purposes.
Creation verifies the current installation key, enabled remote registration,
enrolled nonretired stream and known original regional incident. The connection
need not be online. A duplicate logical issuance reuses its first timestamp and
bytes; changed target/actor/note/lifetime conflicts.

Dispatch serializes under the current database session/owner authority, commits
attempt state first and checks that the remaining lease covers its five-second
operation deadline. Retry delay grows from one second to a maximum of sixty.
Unconfirmed requests continue retrying past expiry because an earlier source
effect may have lost its result. A source with no receipt returns durable expired;
a source with a receipt returns the original outcome. Result storage checks current
session authority and cannot replace a contradictory terminal result.

The command queue is bounded per probe: 16,384 retained rows, 1024 unconfirmed
commands and 64 MiB of protected payload. Confirmed rows can be reclaimed only
after expiry plus 365 days. Unconfirmed results are preserved under pressure.
Downgrade refuses retained protected commands. MariaDB explicitly retains the
index supporting the existing probe foreign key through down/up migration.

`phoenix-probe-admin ack` and `command-status` provide the concrete operator path.
An explicit command UUID permits retries after losing CLI output. Requests target
the original incident/generation, and metadata-only JSON distinguishes pending
from source-confirmed outcomes. The CLI prints a pending warning that alerts may
continue. Notes use optional private bounded files; neither note nor protected
payload is returned. Existing local DB/key access supplies operator authority.
No new HTTP permission dimension or remote notification link is introduced.

`HubTransport` sends only to peers advertising `command.alert_ack.v1`. Its bounded
sender is canceled and joined with the session. Older peers retain existing
health/config/replay behavior with commands pending. `EdgeRuntime` executes under
the current authenticated identity and returns a validated control-frame receipt.
Raw payload value bytes are preserved, including internal JSON whitespace; the
transient connection generation is not part of immutable command identity.

New ACK replay is authorized inside the same transaction as its incident mirror,
receipt and cursor. It requires the exact issued/dispatched protected command,
original source identity/generation, actor/note, accepted opening and consecutive
transition version. Pending commands can authorize telemetry before the control
result arrives. Telemetry never sets `remote_confirmed`. A closed assignment does
not redirect or invalidate its issued original-incident ACK. Recovery retains
accepted ACK metadata and validates membership in its exact retained config;
monotonic transition order survives a backward source clock. Watchdog ACK remains
unsupported by the positive-assignment-generation target and fails closed.

## Focused and process verification

Commands executed: focused `go test -race -count=1` command storage/replay matrices,
core policy/codec checks, the actual TLS command regression, and
`scripts/probe_runtime_smoke.py --verify-replay --verify-command` using freshly
built app/probe/admin binaries and a new disposable local MariaDB schema.

Engines and named tests exercised:

- `TestProbeCommandStorage/{sqlite,mariadb}`: immutable encrypted issuance/results,
  competing claims, owner/generation/stream/key/lease fences, late rollback,
  expired-request retry and guarded down/up migrations.
- `TestProbeCommandReplay/{sqlite,mariadb}`: both result/telemetry arrival orders,
  closed assignments, duplicate replay, preserved recovery after clock rollback,
  unissued/unsent commands, forged actor/note, foreign stream, rejected result,
  corrupt protection, final-write rollback and concurrent logical issuance.
- `TestAccessReplayAcknowledgementRequiresIssuedOriginalIncident` and
  `TestAcknowledgementCodecPreservesExactPayload`: lifecycle policy, exact bytes,
  invalid data and missing/nullable target generation.
- `TestCommandRuntimeExactBytesReceiptLossAndRestart`: real pinned TLS transports
  and edge SQLite; application interrupted before its effect, source result whose
  hub callback fails, store/runtime reopening, exact result recovery after expiry,
  and changed-byte conflict. The hub dispatcher in this test is a controlled
  double; the real hub storage and complete process path are exercised separately.

Passed / failed / skipped: focused storage rerun 23 named passes; focused replay
rerun 25 named passes, zero skips/failures. These counts include suite parents and
subtests. The actual TLS regression passed under the race detector. Initial
migration, trigger-cleanup and invalid-handshake-fixture failures are documented
with their fixes in [the retrospective](../postmortems/2026-09-21-m3-integration.md#migrations-and-fixtures).

The compiled-process run passed all 35 stages. Its command link partition lasted
15.007 seconds. The queued ACK stayed pending while offline, survived both hub
workers and edge restarting, then produced one source receipt and one accepted
ACK transition. Recovery retained its command/actor, a later outage had a new
source ID, and a new ACK of the resolved original returned `already_resolved`
without touching that later outage. All command-era history replayed with zero
rejection receipts and zero hub send intents for the remote probe. Final source
and committed cursors were both 82. Graceful harness teardown completed.

Acceptance criteria still unverified: credential/certificate prepare/activate
rotation, explicit stream reset, complete queue-pressure/shutdown-flush acceptance,
and the real fifteen-minute partition including fresh state ahead of backlog and
the remaining whole-M3 assertions. This short process test does not replace that
scenario. Existing source delivery permits provider duplicates if a provider
accepted a request but its local receipt was lost; no exactly-once provider claim
is made. No frontend, Helm or dependency changes are part of this increment.

## Review disposition

Antigravity performed read-only bounded audits and owned no files. Its dispatch
review reported no concrete defects. Its later integration review proposed three
changes based on missing callee assumptions: treating the stream as transient,
using current rather than retained config authority, and assuming a nullable
generation reached a dereference. Codex traced and supplied the real callers and
validators. Antigravity withdrew all three findings and acknowledged the lessons.
None of the proposed weakening changes was applied. Test execution and acceptance
remain Codex's independently verified work.

## Final repository gate

Commands executed:
- `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...`
- `GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout=20m -json ./...`
- `golangci-lint run`, `git diff --check`, and a core boundary import scan.

Engines and named tests exercised: full repository suite with the real edge
SQLite store and both SQLite/MariaDB hub adapters. `TEST_MARIADB_DSN` selected the
disposable live MariaDB schema; secrets were not logged. The focused command
storage/replay cases above are included in this full run.

Passed / failed / skipped: build passed; all 22 tested packages passed with
3,398 named parent/subtest pass events, zero failures, 264 MariaDB-named passes
(including 227 lowercase `/mariadb` events) and zero MariaDB skips. Lint reported
zero issues. The two existing named skips are
`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`; thirteen packages have no tests.

All 33 changed Go/SQL hashes remained unchanged throughout verification. The
aggregate runner's final checksum guard exited 1 because the smoke harness's
expected unacknowledged `ack_command_id` was corrected from empty string to SQL
null while the Go suite ran. This was a harness assertion correction, not a Go
change or a failing Go test. The final harness then passed all 35 real-process
stages. The original and final hashes, successful individual gate exit codes,
process report and this guard discrepancy are retained in
[M3_COMMAND_EVIDENCE.json](M3_COMMAND_EVIDENCE.json).

Acceptance criteria still unverified: the remaining whole-M3 requirements listed
above. No push or deployment was performed.
