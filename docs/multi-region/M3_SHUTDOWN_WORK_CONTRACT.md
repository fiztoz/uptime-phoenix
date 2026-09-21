# M3 pressure and bounded shutdown work contract

Date: 2026-09-21. Implemented after hub-reset commit `98f136a`. This document
records the design contract; [acceptance](M3_SHUTDOWN_ACCEPTANCE.md) contains
executed evidence and the remaining whole-M3 limits.
Codex owns all files, tests and commits. Antigravity has no file ownership.

## Existing path to preserve

`cmd/probe/runtime.go` currently derives scheduler, delivery, retention, watchdog
and HTTP session contexts from one cancelable context. On shutdown it cancels all
of them before closing the runtime and waiting for workers. Telemetry is durable,
but no bounded opportunity remains to deliver the final committed prefix.

`EdgeRuntime.Close` prevents new admissions, closes every handshake/session and
joins handlers before the database closes. Preserve that ownership ordering.
`edgeReplayPump` alone consumes validated ACKs and commits the local cursor; do not
add a second sender or a shutdown shortcut that prunes on send. Current snapshots
must still precede backlog and must never advance the historical cursor.

`Store.ReadDiagnostics` already reports queue bytes, oldest queued timestamp,
retention gaps and 80% pressure. `serveEdge` exposes `queue_pressure`,
`telemetry_gap`, storage and scheduler errors through application health. Verify
these actual paths before adding diagnostics or claiming they are absent.

## Required behavior

Stop admitting new work on termination. Bound existing checks/provider attempts,
preserve completed results and join producers before deciding which durable
telemetry prefix can be flushed. Keep transport/ACK processing alive for a finite
best-effort interval under its own shutdown context. Preserve existing session,
installation, credential/pin and stream fences throughout the interval.

Choose and document a fixed deadline below the deployment's termination grace.
A missing/disconnected hub, withheld ACK, storage failure, expiration or lost
lease must not turn shutdown into an indefinite wait. Unacknowledged bytes stay
durable for ordinary replay after restart. Report bounded nonsecret drain outcome
and remaining work; never report flush success from an empty memory queue or a
successful socket write.

Trace management mutations while draining: new admission, config replacement,
command application, rotations and watchdog/provider work cannot keep creating
an unbounded moving target. Separate producer quiescence from session close
without creating a second running identity or weakening storage authorization.
Do not close storage while callbacks can still reach it. Retain existing finite
I/O deadlines; verify cancellation of the supported concrete adapters rather than
claiming a goroutine can forcibly stop arbitrary code that ignores context.

## Acceptance

- Real SQLite and real transport: pending final telemetry drains to a matching
  durable ACK when the hub is healthy; exact prefix survives lost/withheld ACK.
- Offline shutdown finishes within the documented bound and cold restart retains
  stream, counters, accepted config and original event/command bytes.
- In-flight checks/provider calls stop within supported cancellation deadlines;
  no duplicate provider dispatch or result is invented by shutdown.
- No source mutation or new session can prolong draining indefinitely. Stale or
  foreign ACKs cannot prune evidence. Current-state receipt is not flush success.
- Pressure transitions around the configured 80% threshold, gap diagnostics and
  clearing after acknowledged drain are observable through production health.
- Compiled process termination checks, focused race tests and full repository
  gate. Then run an actual 900-second partition with DOWN/UP, source restart,
  original-incident ACK and fresh state ahead of replay under
  [the complete M3 contract](M3_COMPLETION_WORK_CONTRACT.md).

The existing `--command-partition-seconds 900` flag alone does not prove the final
scenario. Its current command block keeps the target DOWN throughout the partition
and recovers it only after reconnect. The earlier DOWN/UP block is a separate short
offline interval. Add explicit evidence of the required transitions inside the
actual long partition, plus current-state visibility before historical backlog
finishes; preserve incident-specific ACK semantics instead of relabeling the old
short workflow as full acceptance.

## Chosen shutdown lifecycle

Use separate session, producer and background contexts. The signal stops new
HTTP admission and management mutation admission. A management barrier waits for
an already-admitted command/config callback; its existing session handler deadline
remains in force. A flag alone does not provide this barrier. Current-state and
telemetry receipts continue through the same authenticated session.

Stop new scheduler checks and provider claims first. Allow existing checks and
provider attempts up to ten seconds to finish and commit their outcomes, then
cancel their contexts and join them. Watchdog and retention work stop and join as
well. `FinishDelivery` creates a `delivery.result` event and advances the source
sequence, so it is a producer too. Cancellation after external provider acceptance
but before the outcome commit retains the existing uncertain-delivery retry
window; shutdown cannot guarantee exactly-once external delivery.

After all producers and management mutations have joined, capture the durable
stream and `last_created_seq`. Let the existing replay pump process matching ACKs
for at most five seconds. Compare durable `committed_seq` against that fixed
prefix, checking the identity remains unchanged. The result is flushed, pending
at deadline, or storage/identity failure. None permits deleting unacknowledged
bytes. Close and join session handlers, shut down HTTP within five seconds, then
release storage and the exclusive data-directory lock. The intended normal bound
is below the default 30-second termination grace; supported concrete adapters
must honor cancellation, and tests must demonstrate that behavior.

Antigravity's advisory review confirmed the existing shared-context problem and
the need to join producers. Codex corrected its false claim that finishing a
delivery produces no telemetry and its insufficient flag-only mutation barrier.
The advisory report is not executed acceptance evidence.
