# M3 integration retrospective

## Summary

M3 verification exposed defects in transaction ordering, lifecycle cancellation,
timestamp handling, retention and test fixtures. They were reproduced and fixed
before milestone acceptance; this record does not claim a production incident.
Codex owned integration and verification. Antigravity supplied bounded implementation
slices and advisory reviews; findings were accepted only after tracing or reproduction.
The accepted implementation is `a12a3fa`, with the final record in `8b455d4`.
See [M3 acceptance](../multi-region/M3_COMPLETION_ACCEPTANCE.md) for scope and
[hashed evidence](../multi-region/M3_COMPLETION_EVIDENCE.json) for executed checks.

This consolidates the checkpoint retrospectives. Their original narratives and
work ledgers are preserved in the local archive described in
[documentation maintenance](../multi-region/DOCUMENTATION.md).

## Root cause and fix

### Assignment and authority

Configuration publication held the local registration while reading assignment
sets; replacement held an assignment set while its foreign-key update needed the
local registration. Two active workers and target setup reproduced MariaDB error
1213. `ProbeAssignmentStore.Replace` now locks the sorted union of old/new/local
registrations before the set, then rechecks the immutable expected revision before
child writes. Bounded retries supplement that order; they do not replace it.
Commit `e1dea37` contains the fix and
[deterministic two-connection regression](../../internal/adapters/repository/config_lock_order_test.go).

Runtime ownership and connection generation have different lifetimes. Putting
enrollment under an already-running worker's runtime lease blocked initial pairing.
Enrollment now rechecks prepared authority without acquiring that runtime lease.
Normal renewal cleanup also incorrectly replaced a successful result with
`context.Canceled`; genuine renewal failures still cancel and join the session.
`RenewRuntime` retains `max(stored deadline, DB now + duration)` so clock rollback
cannot shorten parent authority beneath committed child deadlines. Matched release
may clean up an expired owner; it must never release its successor.
[Ownership evidence](../multi-region/M3_RUNTIME_ACCEPTANCE.md) distinguishes these
effects from a cached lease value or an isolated timer test.

### Replay and history

The first replay loop slept inside event handling and could starve ACKs or resend
on duplicate ACKs. Scheduled sends, bounded feedback and joined session workers
keep receipt processing live. Exact retained bytes identify retries; normalized
storage timestamps identify lifecycle state. Those are separate comparisons.
Authorization reads the event's exact retained configuration and assignment inside
the write transaction. Mirroring an edge event never grants provider-send authority.
See [replay regressions](../../internal/adapters/probe/replay_acceptance_test.go).

Queue ordering did not stop a parent history bucket from consuming a new dirty
token before its child committed. `requireCleanHistoryChildren` now checks
dependencies in the projection snapshot; the final lock/token check covers later
writes. Random work tokens avoid counter reuse. Persisted repair ranges handle a
later sequence with an earlier timestamp beyond the usual freshness window.
Background and on-demand readers now share coherent evidence, and parent statistics
retain legacy samples without inventing known duration. Assignment removal itself
invalidates history. [History acceptance](../multi-region/M3_HISTORY_ACCEPTANCE.md)
records the real-engine barriers, rollback and reader-comparison cases.

### Watchdogs and clocks

An early validator allowed recovery to invent ACK metadata, and a later healthy
checkpoint could enqueue recovery again. Source validation now preserves only
committed ACK identity and requires the resolution transition in the same commit
as its recovery intent. Existing resolved UUIDs prevent identity reuse; their
presence does not mean an incident is still open.

Hub incident identity initially compared original nanoseconds to persisted
microseconds, rejecting valid retries. Normalize to the affected schema's precision
without mutating caller data. Watchdog delivery claims also mixed the caller clock
with database-time authorization: about 35 ms of skew rejected a valid MariaDB
claim. Claim, expiry and authorization now use the same database clock. Provider
payload tests separately corrected urgent Gotify recovery and overlong Slack headers.
[Source](../multi-region/M3_WATCHDOG_SOURCE_ACCEPTANCE.md),
[hub](../multi-region/M3_HUB_WATCHDOG_ACCEPTANCE.md) and
[delivery](../multi-region/M3_WATCHDOG_DELIVERY_ACCEPTANCE.md) records retain the tests.

### Credentials and certificates

A successful socket write was followed by source teardown, canceling the hub's
asynchronous receipt transaction repeatedly. The source now quiesces effects while
the hub commits and requests reconnect, with a bounded fallback. Cleanup retains
preparation bodies referenced by unresolved activation. Observed credential-overlap
expiry is retired durably on both authentication and command paths, so a new command
after clock rollback cannot revive it. [Credential acceptance](../multi-region/M3_HUB_CREDENTIAL_ACCEPTANCE.md)
covers both-store restart and lost-result recovery.

TLS storage assumed `tls.Certificate.Leaf` was populated under every supported
toolchain setting. Explicit DER parsing fixes the reproduced compatibility-mode
panic. Runtime tests then exposed first preparation on an already-admitted socket,
historical receipt recovery, redundant concurrent cache reloads and standalone
codecs bypassing strict JSON validation. Fixes preserve the existing admission
deadline, distinguish fresh preparation from recovered receipts, double-check cache
recovery under its gate and validate original JSON before decoding typed commands.
[TLS acceptance](../multi-region/M3_CERTIFICATE_RUNTIME_ACCEPTANCE.md) records the
actual pinned-socket tests and negative controls.

Hub receipt storage omitted UTC normalization for the new certificate expiry,
shifting a UTC+7 value by seven hours on MariaDB. A certificate-only peer also never
started command dispatch because the condition checked only ACK/credential capability.
Normalize every persisted timestamp at its boundary and test capability subsets,
not only the fully enabled edge. [Hub certificate acceptance](../multi-region/M3_HUB_CERTIFICATE_ACCEPTANCE.md)
covers both reproduced failures.

### Reset and archive durability

An exact reset retry verified a visible archive but skipped syncing its publication
parent. After a failed parent fsync, retry could delete live evidence without
establishing archive durability. `verifyResetArchive` now repeats all required
syncs on reuse before live state changes. Keep the same sync failure active during
retry to prove preservation. [Source reset acceptance](../multi-region/M3_SOURCE_RESET_ACCEPTANCE.md)
records that regression.

Hub reset originally reused a query across rotation schemas with different columns.
Real-engine tests caught it before reservation. Local command cancellation remains
separate from an authenticated remote result. The process fixture also leaked
SQLite readers and created WAL sidecars while inspecting an archive. Explicit
connection closure and immutable archive reads fixed the harness; exclusive recovery
locking and archive validation stayed intact. See
[hub reset acceptance](../multi-region/M3_HUB_RESET_ACCEPTANCE.md).

### Storage and shutdown

Telemetry retention left configuration revisions, resolved incidents and terminal
deliveries growing indefinitely. Real SQLite reproductions retained all 64 configs
and 32 resolved incidents after a 400-day sweep. Commit `82cfbb4` adds bounded
reference-aware cleanup, a separate metadata quota and durable generation tombstones.
Current state, unresolved incidents, pending work and exact queued telemetry survive.
SQLite page limits and serialized checkpoint admission address physical growth;
the WAL threshold permits one admitted transaction and is not a filesystem quota.
[Storage acceptance](../multi-region/M3_STORAGE_BOUNDS_ACCEPTANCE.md) retains quota,
rollback, pinned-reader and repeated-file-cycle measurements.

Canceling producers and transport together left no final ACK-drain opportunity.
Shutdown now stops admission, joins producers, drains a fixed prefix within its
deadline and keeps unacknowledged bytes. Concrete SMTP tracing also found that a
nominal dial timeout did not bound the server greeting and caller cancellation was
ignored. Commit `728aefd` installs a context-aware socket deadline before greeting
or TLS and removes transport retries outside the durable dispatcher.
[SMTP regressions](../../internal/adapters/notifier/smtp_shutdown_test.go) use a
silent local peer. External acceptance before a failed local outcome remains
ambiguous; no exactly-once provider guarantee is claimed.

### Migrations and fixtures

Older migration rehearsals rebuilt only a hand-maintained suffix, leaving the
actual schema behind its unchanged migration ledger. Discover the full suffix,
restore in dependency order, and compare usable columns after cleanup. Physical
column order is not column identity. MariaDB may reuse a covering index for an FK,
so downgrade must preserve an explicit supporting index and tolerate partial DDL.
Register fault-trigger cleanup immediately; target the intended late statement
rather than the initial no-op lock. See
[migration evidence](../multi-region/M3_MIGRATION_REHEARSAL_EVIDENCE.json) and
[command acceptance](../multi-region/M3_COMMAND_ACCEPTANCE.md).

Transport fixtures must satisfy real retained-config, identity and encoding
contracts. Fix fixtures rather than relaxing production guards. Reset eligibility
uses database time, so its fixture now anchors evidence to persisted confirmation
instead of the host clock (`046e4cc`). The first final partition harness counted
legitimate hub-owned watchdog sends as forbidden mirrored sends. Commit `a12a3fa`
uses durable incident ownership and preserves comparison input before ACK pruning.
The failed runs remain separate from accepted evidence.

## Why these escaped initial checks

Mocks concealed SQL wall-clock precision and InnoDB locks. Enrollment-first process
tests missed workers already running. Happy lifecycle tests omitted forbidden
transitions, duplicate recovery and unchanged-state side effects. Isolated storage
tests did not prove transport teardown, TLS selection or provider cancellation.
Fresh SQLite files hid damaged shared-schema cleanup. An ignored MariaDB environment
variable once produced a green suite with engine skips; corrected evidence explicitly
checks `TEST_MARIADB_DSN` and executed engine cases.

## Validation

The final accepted source passed CGO-free build, lint and the full race suite:
3,719 named passes across 22 packages, zero failures and two optional skips.
The evidence identifies 303 executed live MariaDB cases. The 900.005-second partition
passed 51 compiled-process stages and replayed 1,440 retained events, preserving
1,434 observation timestamps, fresh state before backlog and one original ACK.
Credential/certificate/reset process acceptances are separate linked records.
These are the recorded implementation results, not new executions during this
documentation consolidation. [Final evidence](../multi-region/M3_COMPLETION_EVIDENCE.json)
contains exact hashes and coverage boundaries.

## Review practice and follow-up

Read actual ports, callers, transaction order and schema before asserting a defect.
Missing context in an excerpt is an inspection gap. Preserve the distinction among
source commit, wire transmission, peer authentication and durable receipt. A tool's
exit code or a confident review is not execution evidence; inspect its result status,
actual output and tests. Antigravity acknowledged corrections to unsupported claims.
Workflow commands such as `/learn` do not expand assigned file ownership.

The final M2 review's health-read, generation-lock and uncertain-send concerns did
not reproduce safety defects: bounded reconnect, atomic generation publication and
receipt/idempotency handling already applied. The useful follow-through was the
real missing configuration-receipt deadline, implemented and verified in
[configuration acceptance](../multi-region/M3_CONFIG_SYNC_ACCEPTANCE.md).

The new [agent rules](../../AGENTS.md) retain these general obligations; the committed
[testing guide](../TESTING.md) carries runnable procedures. M4 compatibility and M5
fleet UI remain in the [implementation plan](../multi-region/IMPLEMENTATION_PLAN.md).
There is no new unresolved defect asserted by this retrospective.
