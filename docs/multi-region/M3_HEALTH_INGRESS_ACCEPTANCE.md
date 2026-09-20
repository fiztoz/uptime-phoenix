# M3 health ingress verification and retrospective

## Summary

The runtime reader used to execute replay, current-state and configuration callbacks
before reading another WebSocket frame. A slow storage callback therefore delayed
otherwise valid application health. `Session.RunWithHealth` now reads/validates
frames independently and feeds two bounded ordered workers. The hub and edge
production transports both use it. This fixes health ingress; it does not enable
or complete either watchdog, provider reconciliation or the whole M3 milestone.

## Root cause and reproduced failure

`Session.readerLoop` called its supplied handler synchronously. The hub handler
could wait in replay ingestion or durable config-receipt storage; the edge handler
could wait in configuration application. Outgoing control priority could not help:
the next incoming health frame had not been read yet. The regression
`TestSessionHealthProgressesDuringBlockedWork` failed against the original serial
path in 0.50 seconds. Its non-health handler waited for cancellation while a valid
health frame remained undelivered. The original failure is retained in the evidence
log. The corresponding new dispatch path passes without releasing the slow work.

## Fix and boundaries

The reader captures `time.Now()` at read completion, preserving its monotonic
component, then validates envelope, generation, payload and peer role before
admission. `HealthReceipt` carries that receipt time and generation. The health
worker retains sample order, including unhealthy frames. The ordinary worker
retains non-health frame order. Queues are bounded to four health samples and
sixteen ordinary frames (each subject to the existing one-MiB frame limit).
Overflow closes the session, so it cannot silently erase a degraded sample or
manufacture a healthy recovery streak. Slow callbacks receive bounded contexts;
all callbacks are joined before transport teardown returns.

`Session.Run` remains serial for callers that require the original contract.
`RunWithHealth` is single-use too; a rejected second run cannot cancel the first.
The hub's health callback continues to use the exact connector lease captured by
`connectOnce`. Durable config receipts and telemetry ACKs still follow their
respective commits. A health callback does not itself acknowledge telemetry.
Existing edge replay/current-state health setters already synchronize their state.

This change introduces concurrency between the two callback lanes, so shared
mutable callback state must be synchronized. Production call sites use their
existing atomics/mutexes and join before reading callback results. Callbacks must
honor context cancellation; Go cannot safely kill arbitrary non-cooperative code.
No asynchronous provider work is introduced here.

## Validation

Tests exercise blocked ordinary work, blocked health work, preservation of
healthy/unhealthy/healthy sample order, monotonic receipt time before dequeue,
overflow cancellation and worker join, repeated Run, and rejection of wrong roles
and stale generations before application callbacks. The real TLS test
`TestHubHealthConfirmsConfigImmediatelyAfterDurableReceipt` now also holds the
config receipt transaction callback open and confirms a subsequent health frame
refreshes authority while that work remains blocked. Existing current-state,
replay/retry, enrollment, fencing and edge runtime tests remain part of the complete
transport race suite.

The final full Go race suite passed in 21 test packages, with 226 MariaDB-named
pass events and zero MariaDB skips. CGO-free build and lint (zero issues) passed.
All six changed Go files match the source hashes frozen before the final gate.

The final executed gates, source hashes and Antigravity audit disposition are
recorded in `M3_HEALTH_INGRESS_EVIDENCE.json`. This document must not be interpreted
as process-watchdog acceptance.

```text
Commands executed: go test -race -count=1 -timeout=20m -json ./...; CGO_ENABLED=0 go build ./...; golangci-lint run (Go 1.26.6).
Engines and named tests exercised: real in-memory WebSocket pairs, local TLS hub/edge runtime, SQLite and explicitly configured disposable MariaDB in the full suite.
Passed / failed / skipped: original blocked-reader and alias regressions failed, then passed after fixes; final suite 21 package passes, 226 MariaDB-named passes, zero MariaDB skips; build/lint passed.
Acceptance criteria still unverified: timer/config/provider integration, durable commands and offline ACK, credential/certificate rotation, operator reset, bounded cleanup/shutdown flush, and the final 15-minute partition.
```

## Audit regression: duplicate JSON decoding changed validated health

The first draft carried forward the edge runtime's raw health decoding pattern:
it decoded the payload into a Go struct after `DecodeHealth` had already
validated exact protocol fields. Go's standard JSON
struct decoder matches keys case-insensitively. A frame containing
`"ready": false, "READY": true` therefore passed the exact-field validator as
degraded but reached the draft callback as healthy. Antigravity questioned the
second decode; Codex found this concrete counterexample and reproduced it with
`TestSessionHealthUsesValidatedFields`. The callback now receives the exact
`Health` object produced by validation, with no second raw-payload decode. This
also removes duplicate parsing. The original alias regression failed, then passed
with all other focused health tests after the fix.

Antigravity also called cancellation of queued uncommitted frames permanent loss.
That claim does not match the durable protocol: queue admission is not a receipt,
the source retains unacknowledged events/config state, and reconnect retries under
a new session fence. Draining canceled-session work with replacement authority
would weaken cancellation/fencing. The existing retry/lost-receipt contracts stay
in the gate. Its separate silent-close claim did not reproduce in twenty direct
internal-close runs or the existing real config-deadline test; it is not recorded
as a demonstrated defect. The direct-close regression is retained to guard the
claimed behavior.

## Why it slipped through and review lessons

Existing transport tests established outgoing control priority and durable receipts,
but did not hold an incoming data callback open while requiring a later incoming
health callback to progress. Treat each traffic direction independently. Use actual
receipt monotonic time; a timestamp captured when a queued worker runs shifts the
outage clock. A full queue cannot safely accept a synthetic replacement sample:
close the session and break stabilization instead of dropping old health.

Antigravity's read-only design audit identified the same incoming blocking path.
Codex reproduced it, implemented and tested the change, and retained ownership of
all source. The audit's broader suggestions were corrected: parent renewal must
follow durable checkpoint progress rather than evaluation alone, and the existing
connector callback already captures its exact lease generation. Those runtime
integration requirements remain open; no false completion claim is made.
