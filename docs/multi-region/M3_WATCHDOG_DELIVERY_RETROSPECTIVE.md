# Watchdog delivery retrospective

Owner: Codex integration, branch `codex/multi-region-probe-plan`, based on
`32dc35e`. Antigravity supplied a read-only provider review. The fixes were found
before enabling watchdog configuration; no production watchdog outage is claimed.

## Summary

The new watchdog send authorization exposed a mixed-clock lease bug in the shared
hub outbox. A valid MariaDB claim was rejected because the application clock was
about 35 ms ahead of the database clock. Provider request assertions also exposed
urgent Gotify recovery notifications and oversized Slack headers. Hub watchdog
leases now use database time consistently, recovery priority is zero, and Slack
probe headers fit the documented limit while the body retains the full name.

## Root cause and mechanism

`RegionalCommitStore.ClaimDeliveries` used the caller's wall clock for `leased_at`
and `lease_until`. `ProbeWatchdogStore.AuthorizeWatchdogDelivery` correctly checks
the stored claim and parent owner using `replayDatabaseTime`. When the application
clock led MariaDB, the new claim appeared to begin in the database's future.
Ordinary cross-host skew therefore blocked a legitimate send. Extending the lease
or sleeping would mask the symptom without making the two authority clocks agree.

`GotifySender.Send` put probe events through the existing auxiliary branch, which
used priority 8 regardless of recovery status. `SlackSender.Send` composed a probe
header from an unrestricted display name; a valid 200-character name produced a
225-character header. A permissive local server accepted both payloads. HTTP 200
alone did not verify recovery semantics or the vendor limit. Slack documents a
[150-character header limit](https://docs.slack.dev/reference/block-kit/blocks/header-block/).

## Fix

Watchdog claim selection, expiry/reclaim, lease timestamps and finish-time lease
validation now use the database clock. Stored delivery outcome time remains the
source wall clock; it is historical evidence, not authority. Existing non-watchdog
outbox clock semantics are preserved. The provider service uses one ten-second
context for authorization and sending, and storage requires both claim and parent
leases to cover that budget. No database lock spans external I/O.

Probe recovery uses Gotify priority zero. Slack truncates only the probe header by
Unicode code points, appending an ellipsis within the 150-character limit. Explicit
probe name/location/source identity flows through templates and webhook output;
all eleven providers render connection loss/recovery without fabricated monitors.

## Investigation ledger

1. The first focused real-engine run passed SQLite authorization but failed
   `TestHubWatchdogSendAuthorization/mariadb` with a valid claim. The MariaDB-only
   reproduction failed three consecutive times.
2. `dlv` was unavailable. Source tracing and temporary sanitized instrumentation
   confirmed matching claim token, stream, parent and incident, then showed the
   database clock behind `leased_at`. No secret configuration was logged.
3. Changing only that stored timestamp to one second earlier allowed the same
   authorization. The differential isolated the clock mismatch from config or
   owner fencing. Temporary instrumentation was removed.
4. Database-derived watchdog claims passed both engines. Regression inputs move
   the caller's claim clock forward one and two hours and still forbid reclaiming
   a live database lease. A finish result with an earlier source timestamp succeeds
   while the database lease is valid.
5. New request assertions reproduced Gotify priority 8 on recovery and Slack's
   225-character header. Both pass after the provider fixes. Every HTTP request is
   redirected to a local test server; SMTP uses a local protocol server.

The failing and differential logs remain under `/private/tmp` with prefix
`phoenix-m3-watchdog-send-clock-`; focused before/after logs and final evidence are
indexed in `M3_WATCHDOG_DELIVERY_EVIDENCE.json`.

## Why it escaped earlier checks

The prior shared outbox did not perform this new database-time send authorization.
SQLite runs on the same host and did not expose cross-host skew. This integration
introduced the incompatible comparison, so the defect belongs to Codex's current
integration work. The provider tests initially proved successful requests without
asserting the load-bearing priority and size constraints.

## Validation and review lessons

`TestHubWatchdogSendAuthorization` exercises actual SQLite and MariaDB claims,
authority/config fences, send-budget coverage, ACK suppression and recovery.
`TestEdgeWatchdogSendAuthorization` covers the edge SQLite equivalent. Core tests
exercise config/channel reconciliation, immutable stored context, redacted retry,
finish failure and cancellation. `TestProbeWatchdogRuntimeDeliveryDoesNotBlockProgressAndIsJoined`
proves timer storage continues while a provider blocks and shutdown joins it.
`TestProbeWatchdogHTTPProviderRequests` covers all ten HTTP senders; SMTP has its
own real request test. The acceptance record distinguishes these executed tests
from the still-unverified enabled process path and remaining M3 requirements.

Antigravity conversation `2099f8ff-b09c-4e17-acfe-f9c8663af768` confirmed the Gotify
issue. Its request to print source UUIDs in every human message was declined:
structured identity and template availability are required, universal display is
not. Its empty-name concern is blocked by the actual delivery service validation.
An unpasted constant was already defined in `probe_regional.go`; absence from a
bounded excerpt did not prove a missing symbol. Antigravity acknowledged these
dispositions and the testing lessons without editing repository files.

Follow-up owner: Codex. Complete exact historical watchdog replay authorization
and enabled both-side process acceptance under `M3_WATCHDOG_WORK_CONTRACT.md`, then
the remaining command/rotation/reset and partition requirements in
`M3_COMPLETION_WORK_CONTRACT.md`. Provider requests can succeed before a process
crashes while persisting their outcome; these changes do not promise exactly-once
external delivery.
