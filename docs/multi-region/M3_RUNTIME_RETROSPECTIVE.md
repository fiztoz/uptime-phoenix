# M3 runtime ownership retrospective

## Summary

Persistent watchdog timing needs one worker to own a probe through reconnect
backoff. The previous connector held only a per-session lease, so independent
workers could alternate attempts and discard an in-memory outage timer on every
connection. The new runtime epoch survives reconnect attempts; a separate session
generation still fences every connection. This is a watchdog prerequisite, not
completed watchdog alerting or completed M3.

The bugs below were found in **Codex's uncommitted ownership draft**, after
history commit `97a0df7`. They are not attributed to the earlier junior agent.
Antigravity reviewed supplied source without tools or file ownership. The fixes
and regression tests are in the same commit as this record.

## Root cause and fixes

### Successful work reported as canceled during cleanup

`withRuntime` starts a lease-renewal goroutine and cancels it after its action
returns. If renewal was inside DB I/O, normal cleanup canceled that I/O; the
renewal goroutine saved `context.Canceled`, and the caller returned that error
instead of its successful action result. Antigravity supplied this concrete
interleaving. `TestProbeConnectorRuntimeCompletionDoesNotBecomeCanceled` reproduces
it with a barrier: renewal starts, the action returns nil, then renewal observes
cancellation. The original test failed with `context canceled`.

The renewal loop now ignores a canceled renewal only when its owned context is
already canceled. A genuine renewal conflict still stops and joins the session,
proved separately by `TestProbeConnectorRuntimeFailureCancelsAndJoinsSession`.
Do not hide all renewal errors or stop checking authority to make cleanup pass.

### A background worker prevented first enrollment

The draft also put operator enrollment under the long-lived runtime lease. A
worker could acquire that lease immediately after registration, repeatedly try
the not-yet-accepted credential, and hold ownership forever while waiting for the
operator. The operator's enrollment needed the same lease: it returned conflict
and never reached the edge. `TestProbeConnectorEnrollmentWhileWorkerOwnsRuntime`
failed with `conflict called=false` before the fix.

Enrollment now revalidates enabled registration and the immutable prepared
credential through `PrepareConnection`, then uses the independent operator
exchange. The edge's `CommitEnrollment` atomically commits its binding and
credential digest and consumes the one-use token. Enrollment neither acquires a
runtime epoch nor advances connection generation. The existing prepared
credential still recovers a lost receipt through runtime authentication.
`TestProbeConnectorEnrollmentRechecksPreparedAuthority` ensures changed
registration cannot reach network I/O.

The original smoke enrolled before starting workers, so it could not expose this
ordering bug. The real process smoke now starts two workers, proves a worker
owns the un-enrolled runtime, then enrolls successfully. It subsequently restarts
the edge and verifies that generation advances within the same runtime epoch.

### A backward DB clock step shortened parent authority

The first `RenewRuntime` assigned `now + 60` unconditionally. A backward clock
step could make this earlier than a child deadline already committed under the
parent epoch. For example, a stored parent deadline of 120 and child deadline of
119 must not become parent deadline 60 after renewal. The draft violated its own
rule that child sessions never outlive runtime ownership.

`TestProbeRuntimeRenewalCannotShortenExistingDeadline` models persisted deadlines
from before the clock step; it changes neither the real DB nor process clock.
The SQLite reproducer failed because renewal shortened the deadline. Renewal
now retains `max(stored deadline, DB now + 60)`. Both-engine validation checks the
same invariant. This concerns DB ownership time; watchdog health duration still
uses measured process-monotonic elapsed time, never subtraction of UTC values.

## Review lessons sent to Antigravity

- Trace actual callers before labeling a method branch a bug. Its proposed stale
  `SetConnectorConnected(false)` finding did not describe production cleanup:
  production calls `ReleaseConnector`, which permits a matched expired release.
- Distinguish an immutable fence from a diagnostic snapshot. Cached
  `ProbeRuntimeLease.LeaseUntil` is not authority; every repository mutation checks
  the current stored owner, epoch and deadline. Updating a shared local lease
  without synchronization would not improve that contract.
- A worker lease spanning reconnects is necessary but does not finish a watchdog.
  Preserve measured loss through a transactional checkpoint, retain open/acked
  incident identity, and re-establish recovery continuity after handoff.
- Test realistic operator ordering: background workers normally already exist
  when another probe is enrolled. A passing enrollment-first smoke is insufficient.

Antigravity acknowledged the verified fix and both rejected findings in
conversation `303f5e7c-bdd8-483f-b416-56e1a0fae260`. No persistent agent rule or
permission was edited, and no external message was sent to a human.

## Validation

The original cancellation and enrollment reproductions now pass in the final
core race suite. The clock reproducer passes after its deadline fix. Both-engine
ownership tests cover independent connections racing for one owner, duplicate
same-worker acquisition, backoff retention, stale callbacks, parent/child expiry,
disable, epoch overflow, transaction rollback and takeover fencing replay.

The final gate and process evidence are recorded in
[M3_RUNTIME_ACCEPTANCE.md](M3_RUNTIME_ACCEPTANCE.md) and its source/hash manifest.
The process test covers both live workers, real enrollment, edge restart,
offline DOWN/provider retry/UP, retained replay and production history work. It is
a short partition test, not the final M3 fifteen-minute acceptance.

## Follow-up

Codex continues with the source watchdog incident/checkpoint/outbox transaction
and complete config/delivery reconciliation in
[M3_WATCHDOG_WORK_CONTRACT.md](M3_WATCHDOG_WORK_CONTRACT.md). Parent renewal must be
coupled to bounded watchdog progress so an otherwise-stalled watchdog cannot keep
ownership merely through an independent renewal goroutine. Both watchdogs remain
disabled until those effects and process acceptance work end to end.
