# M0/M1 local notification cutover retrospective

Date: 2026-09-20. Reviewed branch: `codex/multi-region-probe-plan`, through
`8d83cf6`. Corrective changes are in the working tree based on that commit.
No production incident or deployed impact was established during this review.

## Summary

The branch claimed M1 completion after adding installation identity, activation,
execution revision stamps, atomic lifecycle recording, and a delivery consumer.
The connected runtime could suppress every ordinary availability notification:
bootstrap disabled the existing dispatcher without preparing an active configuration
that the replacement consumer required. Other failures affected reminders,
maintenance transitions, acknowledgement, recovery summaries, upgrades, and key
ownership. The correction restores the established default dispatch path, repairs
the internal foundations, and withdraws the completion claim. M0/M1 remain in
progress; this correction does not enable a remote probe or complete the cutover.

## Symptoms and reproduction

The existing full race suite passed during review. A separate integration harness
using real SQLite repositories, real services, and a recording provider reproduced
eight failures. Those scenarios now live in
[`TestLocalDeliveryContract`](../../internal/adapters/repository/local_delivery_integration_test.go),
with both SQLite and MariaDB variants. A recovery-after-ack recording test initially
passed as a control; it now also verifies the recovery provider call.

| Trigger | Observed failure before correction | Regression assertion |
|---|---|---|
| Default startup, first DOWN | Zero provider sends; queued work superseded for missing active configuration | `DefaultAvailabilitySend`: one actual send |
| DOWN → maintenance → DOWN | New incident UUID conflicted with the still-open alert; heartbeat transaction rolled back | `DownAfterMaintenance`: observation persists |
| Existing database with original 045 | Canceling an unclaimed delivery violated its existing CHECK constraint | `Existing045Upgrade`: preserve leases, allow cancellation, guard downgrade |
| Edit channel after queueing revision 1 | Consumer sent with the new, unapplied settings | `ChannelEditRequiresNewAppliedVersion`: no send with changed settings |
| Still DOWN after resend interval | Dispatcher reminders disabled; producer created no replacement intent | `ResendAfterInterval`: second provider call |
| ACK after claim, before first send | Attempt 1 bypassed the acknowledgement check | `AckBetweenClaimAndFirstSend`: zero provider calls |
| Recovery while DOWN was leased, summary send times out | Original work was terminal before direct summary I/O; no durable summary retry remained | `DelayedSummaryKeepsDurableRetry`: one UP summary attempt and persisted retry |
| Initialize key A, then prepare using key B | Snapshot writer accepted ciphertext under a different installation key | `RejectForeignSnapshotKey`: write rejected |

The activation review also found a read-to-commit race on MariaDB. The new
[`TestProbeActivationLocksSourceThroughCommit`](../../internal/adapters/repository/probe_activation_lock_test.go)
attempts an ordinary monitor update and tag-link insertion on another connection
*after* the source graph has been read and *before* activation commits.

## Root cause

The changes established individual components without proving their shared runtime
preconditions and ownership boundaries.

- **Startup and cutover (`8d83cf6`).** `run.go` set `SetOutboxDelivery(true)` and
  started the consumer, but never prepared/activated the first local configuration.
  `ReconcileBeforeSend` required `GetActive` to match the queued channel revision.
  Legacy dispatch was silent and replacement dispatch discarded the work.
- **Lifecycle planning (`b03ff9f`).** Planning only important transitions omitted
  still-DOWN reminders. DOWN after maintenance was treated as a new incident even
  though the previous alert remained open. The transaction correctly rejected the
  conflicting identity, but that also rejected the new heartbeat.
- **Upgrade semantics (`b03ff9f`).** Editing migration 045 changed fresh installs;
  migration tracking prevented existing installs from receiving the new constraint.
- **Applied settings (`000584e`, `8d83cf6`).** Revision stamps came from an active
  pointer while monitor, proxy and notification settings came from mutable tables.
  A correct revision number did not prove which settings a checker or sender used.
- **Delivery reconciliation (`8d83cf6`).** ACK suppression was conditional on
  `Attempt > 1`. Recovery summaries bypassed the durable work lifecycle after the
  DOWN intent had been superseded, and inherited the DOWN context rather than a
  durable recovery context.
- **Installation authority (`a03eaf4`).** Startup validation and snapshot writes
  used separate ownership checks. A later writer, or a writer racing initialization,
  could violate the key binding that startup had just verified.
- **Activation atomicity (`41f4d6c`).** Locks on probe/installation rows did not
  constrain ordinary source-table writers. A consistent source read alone could
  become stale before the receipt committed. Tests edited the source before calling
  activation; that did not exercise the read-to-commit interval.

These mechanisms explain why successful builds, HTTP responses, revision fields,
and isolated mock calls were insufficient evidence of working notifications.

## Fix

The [composition root](../../internal/bootstrap/run.go) keeps the existing
notification dispatcher and escalation path active. It no longer wires the
experimental consumer or applied execution into the default runtime. The real-app
smoke verifies this path through actual local HTTP targets and webhook recipients.

The internal applied reader resolves a source graph inside a protected transaction
and compares its encoded hash with the selected immutable snapshot. Execution uses
the captured monitor/proxy/channel/template inputs. Source edits fail closed until
fresh preparation and activation. MariaDB uses explicit SERIALIZABLE isolation;
SQLite takes its writer lock first. Bounded retries handle MariaDB serialization
failures without retrying provider I/O.

Initialization and protected writes now take the same installation lock. Retained
ciphertext verification happens under that lock, and later snapshot writes require
the installed hub/key confirmation. A concurrent initializer/writer test asserts
that incompatible authorities cannot both win. The previous initializer race
assertion was also corrected: its Boolean expression could miss invalid outcomes.

The atomic recorder reuses the open incident, reserves due reminders, and carries
outage identity and start time across maintenance and configuration revisions.
Recovery queues a separate durable summary when no DOWN was recorded sent to that
channel. The consumer suppresses acknowledged DOWN work on the first attempt,
permits recovery, verifies the stored lease, and uses the normal retry/outcome path
for summaries. Superseding old DOWN work performs no provider I/O.

Migration **050** upgrades both engines while preserving source identities and
leases. Migration 045 is restored to its historical definition. The downgrade
rejects attempt-zero cancellations that the old schema cannot represent. MariaDB
requires all writers stopped during the table replacement.

## How it was found

The review traced bootstrap through scheduling, heartbeat persistence, lifecycle
planning, outbox claim, reconciliation, provider call, and completion. Integration
reproducers replaced only the external sender; database behavior remained real.
Provider call counts, captured settings, queue rows, and heartbeat persistence
made the failures observable. MariaDB validation then exposed serialization errors
and migration tests that were downgrading beneath newer foreign-key dependencies.

## Why it slipped through

The tests mostly proved components in isolation. Consumer tests omitted activation
or supplied mocks, so they did not reproduce production's missing active pointer.
The cutover test proved the old sender was disabled, without proving the replacement
sent. Scheduler tests asserted revision/generation fields rather than checker
inputs. Fresh-schema tests could not expose the already-applied migration problem.
A direct summary call was asserted as success without checking durable retry after
provider failure. The activation edit tests did not pause at the contested boundary.

MariaDB cases skip when `TEST_MARIADB_DSN` is absent. A passing command with skipped
cases is not evidence that MariaDB was tested. The review had a passing full race
suite and eight failing connected-path reproductions at the same revision. Race
freedom did not establish behavior or database transaction correctness.

## Validation

- The original connected-path scenarios now pass on SQLite and real MariaDB 11,
  including summary content/retry, lease-preserving upgrade and guarded downgrade.
- Scheduler tests assert the URL actually received by the checker comes from the
  applied definition. Push tests exercise the applied reader. The channel-edit
  regression asserts the provider never receives unapplied settings.
- Additional tests cover revision changes during an open outage, recovery after
  acknowledgement, concurrent installation/key writers, and source writes inside
  activation's read-to-commit interval.
- The full repository race suite passes with `TEST_MARIADB_DSN` set against an
  isolated test database. The focused final regression command is documented in
  [`docs/TESTING.md`](../TESTING.md#local-delivery-cutover-regression-gate-2026-09-20).
- The final `go test -race -count=1 ./...` run passes across all packages.
  This general run skips MariaDB contracts; the separate real-MariaDB suite and
  final focused regressions above supply that coverage.
- `go build ./...` and `golangci-lint run` pass. Lint runs with the project's
  Go 1.26.6 toolchain; the installed linter cannot analyze the host Go 1.27 standard
  library. No dependency change was needed.
- The two-process application smoke passed on its second fresh-database run:
  direct webhooks were **2 per monitor** (DOWN, UP), escalation **1**, later step
  **0**; acknowledgement and restart assertions passed. The first run stopped
  before notification checks at a timing-sensitive scheduling assertion
  (`first=4, second=2` checks in five seconds). That failure is retained, not counted
  as a passing run. Local smoke evidence: `/private/tmp/phoenix-m01-smoke-retry/report.json`.

No remote runtime, real external notification provider, or frontend change was
validated. The provider crash window remains at-least-once: provider acceptance
followed by a crash before outcome commit can cause a duplicate after lease expiry.

## Follow-up and guidance for the next implementation

The remaining work is tracked in the
[corrected implementation status](../multi-region/IMPLEMENTATION_STATUS.md).
No owner or external ticket was assigned during this correction.

Before enabling the consumer, implement and test first activation plus refresh,
inherited and disabled channels, complete escalation behavior, maintenance parity,
restart/reclaim, and source edits through the supported app path. Keep M1 unchecked
until those effects pass on both engines.

For future milestone handoffs, state the exact revision, runtime entry point,
provider effects, database engines actually exercised, and skipped acceptance
work. Reuse these regression tests. Treat a revision stamp, an empty queue, or a
successful status code as intermediate evidence—not proof that the feature works.
