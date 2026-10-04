# Watchdog source delivery verification

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Base commit: `32dc35e`. This increment implements source delivery reconciliation,
probe notification context and both composition roots. It does not enable watchdog
configuration or declare M3 complete. Mirror authorization and enabled both-side
process acceptance remain next.

## Behavior verified

`ProbeWatchdogDeliveryService` loads the exact applied graph, obtains fresh source
authority and asks storage for the authoritative immutable claimed item. Current
channel membership, activity/version, watchdog settings and source lifecycle govern
the send. ACK supersedes DOWN; recovery uses the exact resolved incident and keeps
ACK metadata. Stale caller context cannot replace stored source identity or output.
A single ten-second context covers DB authorization and provider I/O, and both the
claim and hub owner must cover that budget. Storage transactions finish before I/O.
Retries persist redacted error codes and bounded backoff; failed completion is not
reported as durable success. Cancellation leaves the lease to recover normally.

The hub dispatcher is joined with its stable watchdog owner and does not block
source timer persistence. The edge routes watchdog work through its existing
source dispatcher. Source work still uses the existing durable outbox. No new
provider type, external dependency, schema, frontend or Helm change is included.

`AlertScopeProbe` carries probe UUID, name/location and source alert UUID separately
from `DeliveryScope`. Probe template `alert.id` is a UUID string; numeric monitor
and group IDs retain their existing types. The new variables are
`alert.delivery_scope`, `alert.source_id`, `probe.id`, `probe.name`, `probe.location`.
The default webhook contains a probe object and no fabricated monitor. Real local
HTTP request tests cover ten senders, including fixed-URL Telegram/LINE via a test
transport; SMTP is exercised through a local protocol server. Both loss and
recovery carry explicit probe context. Slack header limits and Gotify recovery
priority have direct payload assertions.

Hub watchdog claims, reclaim eligibility and finish-time lease validation now use
database time, matching send authorization. Outcome time remains source history.
The original real-MariaDB mixed-clock failure and provider defects are documented
in [the retrospective](../postmortems/2026-09-21-m3-integration.md#watchdogs-and-clocks).

## Evidence

The final full race suite passed in 22 tested packages with 3,250 named pass events.
It recorded 236 MariaDB-named pass events and 199 lowercase `/mariadb` engine
subtest events, with zero failures and zero MariaDB skips. Counts include parents
and some SQLite tests named after MariaDB regressions; they are not independent
scenario counts. Two existing named skips remain: the opt-in real MongoDB checker
and legacy Telegram hardcoded-URL test. The new redirected Telegram watchdog test
ran and passed. Thirteen packages have no tests.

CGO-free build and zero-issue lint passed. All 25 changed Go file hashes match the
full gate. [Machine-readable evidence](M3_WATCHDOG_DELIVERY_EVIDENCE.json) records
source/log hashes and the exact new test events. The full run also exercises legacy
outbox behavior affected by the shared query change.

```text
Commands executed: go test -race -count=1 -timeout=20m -json ./...;
  CGO_ENABLED=0 go build ./...; golangci-lint run; git diff --check.
Engines and named tests exercised: real SQLite and MariaDB;
  TestHubWatchdogSendAuthorization/{sqlite,mariadb};
  TestEdgeWatchdogSendAuthorization; TestProbeWatchdogDeliveryReconcilesBeforeSend;
  TestEdgeDeliveryDispatchesProbeWithoutMonitor;
  TestProbeWatchdogRuntimeDeliveryDoesNotBlockProgressAndIsJoined;
  TestProbeWatchdogHTTPProviderRequests; TestProbeWatchdogSMTPMessage;
  TestProbeNotificationTemplate* and existing full-suite/outbox contracts.
Passed / failed / skipped: 22 tested packages passed; 0 failures; 0 MariaDB skips;
  2 existing named skips; 13 packages without tests; build passed; lint: 0 issues.
Acceptance criteria still unverified: watchdog mirror replay, enabled both-side
  process delivery, durable commands/offline ACK, rotations/reset, bounded flush
  and the real fifteen-minute partition; final whole-M3 frontend/process gate.
```

This increment has no new enabled process result. Earlier disabled-watchdog process
evidence remains scoped to its original commit. A provider can accept a request
before a process crashes while recording its outcome; exactly-once external sends
are not guaranteed. Historical mirroring must never create hub delivery work.
