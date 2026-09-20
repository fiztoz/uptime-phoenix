# M1 runtime integration follow-up

## Summary

Review of `a2551f8` and its uncommitted corrective patch found that the local
applied-configuration path could omit availability delivery, the proposed correction
could deliver DOWN twice, and escalation could send unapplied settings after a
restart. The integrated correction, committed as `18762e7` on `codex/multi-region-probe-plan`,
uses one availability delivery owner, durable escalation steps, database-backed
configuration authority, and automatic refresh. The real MariaDB smoke also exposed
and verified a correction for a monitor-creation deadlock. M2 subsequently passed
its separate process acceptance; current final verification is recorded in
`docs/multi-region/IMPLEMENTATION_STATUS.md`.

## Symptoms and root cause

- `Run` installed `HeartbeatService.SetActivationRepo` but omitted
  `SetMonitorNotificationRepo`. `persistCheck` gated incident/intents on that field,
  so no producer work reached the running outbox consumer. The fixture supplied
  the missing dependency, making its green result inapplicable to production.
- The corrective patch set `SetOutboxDelivery(false)` while enabling the recorder
  and consumer. One DOWN then reached both legacy dispatch and the outbox.
  A reproduction observed **two provider sends**, not one.
- `CurrentApplied()` cached a process-local pointer, assigned inside an uncommitted
  transaction. Cold instances returned nil. Escalation/notification code fell back
  to mutable channel rows, and those setters were not wired by bootstrap anyway.
  A cold-reader reproduction sent a URL absent from the activated configuration.
- `EscalationService.runOne` sent directly and advanced even on provider failure.
  A successful ladder-state update did not mean delivery had been retained for
  retry. The dispatcher also registered the ladder after heartbeat commit, leaving
  a crash gap between incident creation and escalation registration.
- Refresh subscribed to only a subset of source changes. Adding a full graph read
  every 50 ms hid missing publication but created unnecessary database contention.
- The real consumer configuration lacked `PublicURL`, so configured local
  acknowledgement links were omitted. A manually configured consumer test could
  not establish bootstrap correctness.
- MariaDB monitor creation inserted a monitor before locking the local probe.
  The SERIALIZABLE configuration reader locked the probe before reading monitors.
  InnoDB's deadlock report showed those two waits, and monitor creation returned
  HTTP 500 with error 1213. SQLite-only tests did not exercise this lock order.

## Fix

The recorder now creates incident, alert, initial delivery work and escalation
registration atomically. In configured mode the dispatcher leaves availability
lifecycle to that transaction; the outbox consumer owns provider sends.

`EscalationOutboxStore.CommitEscalationStep` checks the persisted claim, applied
revision/source graph, assignment and firing incident, then inserts step deliveries
and advances the ladder in one transaction. A provider timeout retains a stable
outbox identity and attempt history. The consumer rechecks ACK, recovery, channel,
policy step and maintenance before sending, and reconstructs escalation messages
from applied settings. Migration 051 retains policy/step identity; downgrade refuses
to erase retained escalation work. Durable current state allows telemetry pruning
without losing escalation authority.

Applied readers now consult the database and fail closed on missing/stale state;
there is no cold-cache fallback. Refresh coalesces events into one worker with a
five-second reconciliation fallback and runtime cancellation. Successful HTTP source
mutations publish a refresh hint, and notification service mutations publish events.
Bootstrap supplies the consumer's public URL. Monitor creation locks the local probe
before inserting its monitor, matching configuration lock order.

## Why the previous checks missed it

Tests reconstructed a better pipeline than `Run`, explicitly refreshed after edits,
and kept one reader instance warm. Provider send assertions did not test delivery
retention on failure, transaction rollback, cold restart or a real bootstrap.
MariaDB skips were reported alongside passes. Handoffs also named nonexistent
methods and routes and claimed percentage completion instead of matching acceptance
criteria to observed effects. These were verification gaps, not evidence of working
runtime integration.

## Validation

Independent evidence retained in the local validation directory:

- Original overlay regressions: `/private/tmp/phoenix-m1-integration-review.log`
  (duplicate sends and cold-reader fallback failed before correction).
- `TestEscalationOutboxContract`: SQLite and live MariaDB passed rollback of enqueue
  plus progress, stale-claim rejection, duplicate receipt, provider retry using a new
  consumer, ACK between enqueue/send and a cold reader rejecting unapplied channels.
  Log: `/private/tmp/phoenix-m1-escalation-both-db2.log`.
- The same run passed all 20 SQLite local-delivery contracts. Its MariaDB suite
  exposed a test teardown leaving migration 051 downgraded; after restoring the
  teardown, all 20 MariaDB cases passed in
  `/private/tmp/phoenix-m1-mariadb-regressions.log`.
- `TestMonitorCreateUsesConfigurationLockOrder` passed against live MariaDB in
  `/private/tmp/phoenix-m1-lock-order.log`. It holds the reader's probe lock while
  creation attempts that lock, then reads monitors before releasing the transaction.
- Actual two-process application smoke with `PROBE_SECRET_KEY_FILE` and PublicURL:
  `/private/tmp/phoenix-m1-integrated2-smoke/report.json`. Both workers execute their
  assigned monitor, one DOWN webhook per monitor, one first escalation, absolute
  ACK link, ACK cancellation, restart without resend duplication, and both UP
  recoveries. Webhook totals: direct-first=2, direct-second=2, escalation=1, later=0.

The integrated lint passed with zero issues in
`/private/tmp/phoenix-m1-integrated-lint2.log`. The full race run completed with
failures in the old probe-route text guard, an outdated pinned-client expectation,
and two migration tests that reconstructed an older outbox schema without 051.
The guard now targets unimplemented hub admin routes, while M2 client endpoint
validation is allowed. The migration tests now restore 051 and downgrade it before
050. The corrected complete probe package and both affected migration tests passed
independently (`/private/tmp/phoenix-m2-pinned-independent.log` and
`/private/tmp/phoenix-m1-migration-followup.log`). The 051 rollback guard also passed
on both engines in `/private/tmp/phoenix-m1-escalation-downgrade.log`.

The integrated `make gate-full` subsequently passed Go build, vet, race tests,
lint, Svelte checks/build/lint/tests, 12 browser journeys, Helm validation and
vulnerability checking. Log: `/private/tmp/phoenix-m2-gate-full.log`. The final
CGO-free hub also passed `/private/tmp/phoenix-m1-final-process/report.json`.
M2 has separate enrollment/offline/restart evidence in
`/private/tmp/phoenix-m2-final-process/report.json`.

The additional complete live MariaDB run exposed another teardown omission in
`TestRegionalIncidentsAreIndependentPerProbe`: it rebuilt 045 without restoring
050–051 while the migration ledger still marked them applied. Later tests failed
with missing escalation columns; SQLite's separate file per case hid the leak.
The test now restores both additions and invokes the actual current outbox claim
query to check its restored schema. The enrollment fault trigger now registers
unconditional cleanup so a failed assertion cannot poison subsequent tests.
The final database rerun uses a fresh disposable database because an already
damaged migration ledger is not a clean starting point.

That final complete MariaDB/SQLite repository run passed:
`/private/tmp/phoenix-m2-live-db-final.log`. The additional final full race run
also passed (`/private/tmp/phoenix-m2-final-race.log`), with final lint at zero
issues (`/private/tmp/phoenix-m2-final-lint.log`).

## Lessons and next actions

Codex owns integration and independently runs production bootstrap acceptance.
Antigravity completed its assigned M2 slices and the final audit. The slice review
files record the required corrections and independent verification. Current M3
ownership is defined in `docs/multi-region/M3_CONFIG_SYNC_WORK_CONTRACT.md`;
Codex owns implementation, and any delayed Antigravity response is read-only.

For every completion claim, identify the actual entry point, delivery owner,
transaction boundary and cold-restart behavior. A green helper test is supporting
evidence, not proof that production is wired correctly. Save one full test log and
inspect its exit code; do not repeatedly rerun unchanged tests because output was
truncated. Report skips and outstanding acceptance criteria explicitly.

## Feedback applied during delegation

Antigravity used Gemini 3.8 Flash High for bounded pinned-TLS, identity, session and
edge-delivery persistence slices. Exact file ownership and shared API contracts
kept those edits separate from Codex's bootstrap integration. Codex read the code
and reran tests rather than treating each handoff as a milestone receipt. The M2
review files record corrections for error redaction, private file descriptors and
locking, cancellation races and fair bounded scheduling.

A broad `BEFORE UPDATE` fault trigger on `edge_identity` fired on the initial
no-op writer lock. It did not demonstrate rollback after queue/state inserts.
The integrator's trigger targets `last_created_seq` only when its value changes,
reaching the final counter write after those inserts. An additional pressure test
reserves outcome space before provider I/O and proves observations cannot consume
that reservation. Trace where the injected failure occurs before claiming rollback.

The integrator's first process fixture used UTC `+00:00` instead of the protocol's
required `Z`. The real runtime correctly refused it. Fix the fixture to match
the protocol; do not weaken a valid guard to obtain a green smoke.

When Gemini's quota expired, no overages or model change were enabled. Codex took
explicit ownership of the unfinished runtime slice and replaced the old assignment
with a read-only review contract after reset. This prevents delayed work from
overwriting another agent's files. Report quota errors and pending tool approvals
as pending. Keep test logs in an agreed readable location and use a scoped approval
when needed; do not repeatedly dispatch an unchanged blocked task.

## Final audit and first M3 follow-through

Antigravity completed its final M2 audit. Codex independently reran its focused
runtime/session/connector coverage and traced each concern; see
`docs/multi-region/M2_RUNTIME_SESSION_INTEGRATOR_REVIEW.md`. A suspected issue
must be checked against the actual cancellation and authority boundary before
changing code. A send returning an uncertain result is not evidence that the
writer ignores cancellation; application-level idempotency resolves that boundary.

The M3 receipt trace found a concrete missing deadline: configuration could be
sent successfully and never acknowledged while health kept the session alive.
The connector now requires bounded receipt completion and persists exact receipts
under its current lease. A late database-write failure rolls back both the active
pointer and receipt. The test trigger targets the final receipt insert, after the
pointer write, applying the earlier fault-injection lesson.

M3 coordination hit a different blocker: native app control could read Antigravity
but could not enter a new assignment. Codex did not claim dispatch succeeded. It
changed the shared ownership contract before taking over the encoder; any delayed
Antigravity response is assigned a read-only report. No paid overages, model changes
or concurrent source ownership were used. The M3 implementation is Codex's work.
