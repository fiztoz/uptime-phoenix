# M0/M1 runtime verification and corrective handoff

Date: 2026-09-20. Reviewed baseline: `a2551f8`.

This is the corrective assignment and engineering guidance for Antigravity after
the second local-delivery review. It records open defects, not completed fixes.
Codex owns this document and independent verification. Antigravity owns the
implementation and its regression tests. Keep work uncommitted for review.

## Goal and safe scope

Make the local notification runtime deliver correctly, refresh after supported
source edits, and preserve acknowledgement and escalation behavior. Keep the
zero-key default working. Do not advance M2 or mark M0/M1 complete based on a
count of passing tests. If the applied/outbox path cannot meet these contracts in
this change, keep its production cutover disabled and report the remaining work.

Read AGENTS.md, docs/TESTING.md, the multi-region continuation guide and current
implementation status, and the architecture/protocol contracts before editing.
Preserve the established port-and-adapter boundaries and immutable migrations.
Do not add dependencies, change providers/monitor types, push, deploy, or commit.

## Confirmed review findings

| Finding | Runtime evidence at a2551f8 | Required correction |
|---|---|---|
| P1: missing producer wiring | `internal/bootstrap/run.go` disables legacy availability delivery but never sets the heartbeat service's monitor-notification repository. Matching that wiring produces zero sends and zero intents on initial DOWN. | Establish required dependencies before cutover and exercise the actual composition root. |
| P1: edits leave stale applied state | A normal `NotificationService.Update` is not observed by the refresh subscription. `ReadAppliedLocal` stays in conflict until a manual event. Applied scheduling then cannot proceed. | Reconcile every source dependency mutation with retry and visible errors, including lost-event recovery. Prove normal edits recover without manual refresh or unrelated monitor changes. |
| P1: escalation uses unapplied settings | `EscalationService.RunDue` loads mutable policy/monitor/channel rows and sends directly. Editing the step's channel after activation causes the provider to receive its unapplied URL. | Integrate escalation with applied configuration and durable work/reconciliation before enabling the cutover; preserve suppression, retries and restart behavior. |
| P2: acknowledgement links disappear | Bootstrap passes the default consumer config with empty `PublicURL`; `resolveAckURL` returns empty. | Carry the configured URL into the consumer and assert the resulting provider payload. |

Evidence from the review is in `/private/tmp/phoenix-a255-review.md`. Three failing
SQLite reproductions are in `/private/tmp/phoenix-a255-review-overlay-test.go`,
with overlay configuration `/private/tmp/phoenix-a255-review-overlay.json` and
output `/private/tmp/phoenix-a255-review-repros.log`. These files are temporary
review aids; promote the relevant regression scenarios into maintained tests.
The overlay substitutes a copy of the entire old integration test file, so do not
reuse it after changing that file or treat its copied helpers as current wiring.

## How the tests hid the blockers

The existing 16 SQLite cases and full race suite passed at the reviewed revision.
Build and lint also passed. Three additional runtime reproductions failed.

1. The integration helper supplied `SetMonitorNotificationRepo`; production did
   not. A test assembled from better wiring than the application cannot certify
   startup. Launch the application or reuse the same composition function.
2. The test named `SourceEditsThroughSupportedAppPath` wrote directly to a
   repository and invoked `Refresh` itself. That skips the behavior named in the
   test. Drive ordinary services or HTTP routes and wait for the automatic effect.
3. `CompleteEscalationBehavior` exercised the old direct sender. A successful send
   alone cannot prove the intended queue and applied-version guarantees. Assert
   durable work, actual settings received by the provider, and retry outcomes.

The lesson is to verify each connecting edge, not merely each component. Trace
startup -> scheduler -> heartbeat transaction -> intent -> claim -> reconciliation
-> provider call -> outcome. Identify which test crosses each edge. A test name,
revision stamp, empty queue, or disabled old sender is not an acceptance result.

## Required working method

1. Reproduce each defect before changing it. Record the expected provider effect,
   observed result, exact command and database engine. Add a real acknowledgement
   payload reproduction for the statically traced fourth finding.
2. Explain the failing path and the evidence that would disprove your explanation.
   Change one cause at a time. Keep a short experiment ledger, including failures.
3. State the proposed runtime ownership before implementing escalation: who
   schedules a step, which applied revision supplies dependencies, who persists
   work, who revalidates suppression, and what retries after a crash/failure.
4. Keep diffs cohesive. If delegating further, assign disjoint file ownership and
   a shared contract first; independently inspect and run the integrated result.
5. Repair tests that bypass the failing boundary. Do not weaken assertions, add
   test-only production wiring, or turn source mismatches into successful sends.
6. Use bounded waits with a stated deadline for asynchronous effects. Test a
   transient refresh failure and recovery, so a swallowed error cannot strand
   monitoring. Shutdown must cancel background refresh cleanly.

## Acceptance evidence

- Actual startup with no key still sends DOWN/UP notifications.
- Actual key-configured startup sends initial DOWN through the intended durable
  path; a configured public URL yields a usable acknowledgement link.
- Ordinary edits to every encoded dependency class are eventually applied, and
  checks resume. Cover notifications/links, groups/inheritance, maintenance,
  templates, proxies, escalation and monitor settings as applicable to the source
  graph. Do not invoke refresh manually in the acceptance assertions.
- Edit a channel or policy between activation/claim and provider I/O: no send
  uses unapplied settings. Preserve disabled-channel and maintenance suppression.
- Escalation handles acknowledgement, recovery, provider failure/retry, and
  restart without silently losing pending steps or using the legacy direct sender
  as evidence for durable delivery. Document the at-least-once crash window.
- Run the applicable build, race, lint, formatting and regression gates yourself.
  Run the relevant live MariaDB contracts against an isolated disposable test
  database. Distinguish skipped cases from passed cases; report access blockers
  instead of implying validation. Never use a production database for tests.

## Handoff format

Return the baseline, changed files, each defect's before/after behavior, exact
tests and engines executed, skipped work and remaining risks. Include provider
call counts/payloads, queue outcomes and scheduler progress where relevant. Avoid
unsupported percentage estimates. Keep the status documents consistent with the
observed behavior. Codex will review the diff and independently rerun the decisive
checks before accepting the work.

## Follow-up lesson: fault injection must reach the intended write

The first M2 delivery test used `BEFORE UPDATE ON edge_identity` to claim coverage
of a final-counter failure. `Store.write` first updates `id = id` to acquire the
SQLite writer lock, so that trigger failed before any outcome was written. It
proved an early failure, not rollback after provider-outcome persistence.

The integrator added `TestEdgeDeliveryLateCounterFailureRollsBackOutcome`: its
trigger targets `UPDATE OF last_created_seq` and requires a changed value. It
therefore fires after the outcome event and queue update. The assertions verify
that telemetry bytes/counter remain unchanged and the delivery stays leased with
no outcome timestamp. Use this pattern whenever a transaction has repeated writes
to the same table: identify the exact statement reached by the fault.

Queue tests also need to cross boundaries. A bounded observation queue alone can
leave no space to persist the result of an external provider send. M2 claims now
reserve one maximum event per leased delivery before provider I/O; observations
cannot consume that reserve. A real SQLite pressure test fills the queue, rejects
further observations, and proves the reserved delivery outcome still commits.
This does not promise exactly-once external delivery after an arbitrary crash.
