# M3 source ACK storage and lifecycle acceptance

This increment adds durable incident-specific acknowledgement at the edge storage
boundary. It does **not** enable the operator-to-edge command workflow or complete
M3. The next integration work is documented in
[M3_COMMAND_WORK_CONTRACT.md](M3_COMMAND_WORK_CONTRACT.md).

## Implemented behavior

`Store.ApplyAlertAcknowledgement` serializes with source recording, config and
session changes. It checks authenticated hub/probe/stream and the current durable
connection generation before retrieving a duplicate receipt. The immutable request
hash binds the exact payload digest and decoded target/actor/timestamps, excluding
transient session generation. A repeated UUID with different data conflicts.

One transaction changes the original incident, appends its ACK transition under
one new stream sequence, advances the source counter and stores the immutable
command outcome. Failure at any of those writes rolls everything back. A retry
after response loss, process restart, a new authenticated session or command expiry
returns the original result and application time. Old sessions cannot recover it.

The target is the original source UUID and retained assignment generation. The
current scheduled assignment is not substituted. Resolved targets return
`already_resolved`; a second operator's command on an acknowledged target returns
`already_applied` and preserves the first actor. Missing/wrong-generation targets
return durable `rejected/target_not_found`. Requests have a maximum seven-day
lifetime; a creation timestamp over thirty seconds ahead is rejected. Expired
requests cannot apply after receipt cleanup.

Migration `edge/006_applied_commands` stores digests and nonsecret results, not
request plaintext. Retention is expiry plus 365 days, matching the maximum
configurable telemetry horizon. The ledger is capped at 16,384 rows; pressure
preserves known receipts and rejects new work without applying it. Downgrade
refuses to discard any retained receipt.

`ExpectedIncidentVersion` now fences source check commits independently of the
latest observation sequence. ACK does not fabricate an observation. Checks retry
against the new incident state, suppress resends while acknowledged, retain ACK
metadata through recovery and create a fresh incident on the next outage.
`AuthorizeEdgeDelivery` checks exact active config, source lifecycle, immutable
outbox content and current lease in one transaction at the provider boundary.
Authorization and sending share one ten-second deadline. ACK committed before
that authorization suppresses DOWN; external I/O already authorized may finish.
Recovery remains deliverable after ACK and even after a newer outage opens.

## Executed coverage

The focused race matrix passed 89 named test/subtest events with zero failures or
skips. These counts include parents and subtests, not 89 independent scenarios.
New source cases include:

- `TestEdgeAcknowledgementAtomicRollback`: incident, telemetry, final counter and
  receipt insertion faults, followed by a successful retry.
- `TestEdgeAcknowledgementReceiptRestartAndReconnect`: exact result recovery
  after reopen and expiry; old-generation rejection.
- `TestEdgeAcknowledgementRejectsConflictingIdentity`: raw payload, decoded actor,
  note, target, generation, expiry, hub, probe and stream conflicts.
- `TestEdgeAcknowledgementExpiryTargetAndDuplicateOperators`: terminal outcomes,
  retained old assignments and preservation of the original ACK actor.
- `TestEdgeAcknowledgementConcurrentDuplicate`: eight concurrent requests, one
  source effect/transition and identical receipts.
- `TestEdgeAcknowledgementFencesChecksAndPreservesRecovery`: stale check retry,
  rejected ACK metadata forgery, recovery and a late ACK against a newer outage.
- `TestEdgeAcknowledgementSourceServiceLifecycle`: real service through real
  SQLite source storage, with resend suppression, recovery and a fresh outage.
- `TestEdgeAcknowledgementBoundsAndRetainsReceipts`: full ledger, duplicate
  recovery, safe reclamation and prevention of reapplication after expiry.
- `TestEdgeAcknowledgementMigrationRefusesReceiptLoss` and
  `TestEdgeRegionalDeliveryAuthority`: downgrade refusal, stale/expired claims,
  insufficient send budget, replaced config and acknowledged recovery delivery.

The edge database is SQLite by design. Full-repository MariaDB execution covers
regressions in the existing hub; it does not prove hub command storage, which is
not yet implemented.

## Final verification evidence

Commands executed:
- `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...`
- `GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout=20m -json ./...`
- `golangci-lint run` and `git diff --check`

Engines and named tests exercised: real private edge SQLite for the source cases
above; SQLite and the explicitly configured disposable MariaDB for the complete
hub repository regression suite. The MariaDB DSN was supplied without logging it.

Passed / failed / skipped: build passed, all 22 tested packages passed, 3,326 named
pass events, zero failures, zero MariaDB skips and zero lint issues. There were
241 MariaDB-named pass events, including 204 lowercase `/mariadb` subtest events.
The two existing named skips were `TestDatabaseChecker_Check_MongoDB_RealServer`
and `TestTelegramSender_Send_DownSeverity`; 13 packages have no test files.
All 18 changed Go/SQL file hashes were unchanged through the full gate. No
frontend, Helm or dependency changes were made. No new framework/driver imports
were introduced in core.

Acceptance criteria still unverified: the hub/operator/transport command workflow
and the remaining whole-M3 scenarios listed below. No new process-partition test
was run for this source-only increment. Machine-readable commands, hashes and
counts are in [M3_EDGE_ACK_EVIDENCE.json](M3_EDGE_ACK_EVIDENCE.json).

## Review and coding retrospective

Antigravity performed a bounded read-only review of the exact source. Its first
review correctly located required lifecycle seams already described in the plan,
but incorrectly called two speculative implementation assumptions proven defects:
that a command result needed a second telemetry event, and that reconnect would
reuse the original session generation. Codex corrected both with the actual
control-frame/retry and session-authority contracts. Antigravity acknowledged the
corrections; the implementation review returned no actionable findings. It did
not run tests and owned no source files.

The coding lessons are concrete: keep durable identity separate from current
execution authority; identify every writer of incident state before choosing a
CAS guard; preserve immutable ACK metadata through all later transitions; and
verify source effect plus receipt plus sequence under late-write failure. A
passing source API test does not prove operator/transport/replay integration.

The first focused run found a new test fixture that labeled a sample UP while
retaining DOWN's positive retry count. The existing observation validator rejected
it before recovery storage. Correcting the fixture to the actual UP wire contract
made the intended recovery assertions execute; no production validation was
weakened. The subsequent focused matrix and final gate are recorded separately.

## Remaining acceptance

Hub protected command storage and fencing, dispatch/backoff, edge control-frame
execution, authorized ACK mirror replay and the operator issue/read path remain
unimplemented. So do credential/certificate rotation, explicit stream reset,
bounded shutdown flushing and the final real fifteen-minute partition with an
ACK queued offline. Remote notifications still omit ACK links. Do not infer
whole-M3 completion from this source prerequisite.
