# M3 connector ownership and watchdog timing acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

This increment follows `97a0df7` on `codex/multi-region-probe-plan`. It retains
one hub worker across reconnect attempts and adds the pure watchdog timing
contract. It does **not** enable watchdog incidents or complete M3.

## Behavior

- Hub migration 058 adds `probe_runtime_owners`. Every acquisition advances a
  durable epoch; a live epoch rejects even duplicate loops with the same worker
  identity. The parent lease survives reconnect backoff. Each socket still gets
  a separately increasing generation.
- Parent release/takeover invalidates the child session in the same transaction.
  Child acquisition/renewal cannot outlive the current parent lease. Parent renewal
  cannot shorten a committed deadline after a backward DB clock step.
- The production worker and operator composition roots use the new port. Renewal
  failure cancels and joins socket work before release. Registration removal or
  disable cancels work and storage rejects its subsequent authority.
- First enrollment remains possible with workers already running. It rechecks
  enabled registration and the exact immutable prepared credential, then consumes
  the edge's one-use enrollment token independently of runtime ownership.
- The pure watchdog timer measures monotonic elapsed time, uses 45/90/30-second
  defaults, arms only after explicit activation, preserves pending loss across a
  delayed tick, and requires healthy samples themselves to span recovery. Its
  handoff checkpoint carries measured duration, not a saved wall timestamp or
  partial recovery streak. Runtime persistence/delivery integration is next.

The simpler existing per-connection lease could not cover outage time between
attempts or keep the timer with the health receiver through reconnects. The parent
lease supplies that missing lifetime while preserving existing session fencing.

## Validation ledger

Final `go test -race -count=1 -timeout=20m -json ./...` passed with the real
MariaDB test variable enabled: **202 MariaDB-named pass events and zero MariaDB
skips**. All 21 test-bearing package results passed. The final CGO-free build
passes and `golangci-lint run` reports zero issues.
[M3_RUNTIME_DB_EVIDENCE.json](M3_RUNTIME_DB_EVIDENCE.json) records exact package
results, critical cases, process metrics and hashes for the 19 changed source
files. The final source matches the verified manifest.

Focused regression coverage:

- `TestProbeWatchdog*`: exact timing boundaries, startup arming, degraded health,
  delayed tick, recovery interruptions, restart, handoff and duration limits.
- `TestProbeConnectorRuntime*`: one owner through reconnect backoff, ownership-loss
  cancellation/join, and cleanup that preserves a successful action result.
- `TestProbeConnectorEnrollmentWhileWorkerOwnsRuntime` and
  `TestProbeConnectorEnrollmentRechecksPreparedAuthority`: independent enrollment
  without bypassing a changed registration.
- `TestProbeRuntimeOwnership`, `TestProbeRuntimeAdoptsLegacyAndFencesDisable`,
  `TestProbeRuntimeReleaseRollback`, `TestProbeRuntimeTakeoverFencesReplay`, and
  `TestProbeRuntimeRenewalCannotShortenExistingDeadline`: real SQLite/MariaDB
  contention, transaction rollback, lease clocks and stale replay authority.

No frontend, chart or dependency files changed. Python smoke syntax, whitespace
and core import boundaries are checked. Final M3 still requires the full product
gate in addition to the applicable Go/database/process evidence here.

## Real process acceptance

`scripts/probe_runtime_smoke.py --verify-replay --verify-history` passed with the
actual app, probe and admin binaries, two hub workers and a fresh disposable
MariaDB database. The script now starts workers before enrollment and proves a
worker holds ownership before the operator enrolls the edge.

The parent remained at epoch 1 across an edge restart while session generation
advanced from 3 to 4. The subsequent short offline window retained 21 events;
16 observation sequences, one resolved incident and two successful delivery
outcomes mirrored exactly. The hub created **zero** source send intents. Final
source/ACK cursor was 54. Production workers recomputed a closed minute with 45
source observations, matching count and 60,000,000 microseconds of overall
coverage, with no pending minute work.

Evidence is `/private/tmp/phoenix-m3-runtime-process-acceptance/report.json`; its
hash and sanitized metrics are in the committed evidence file. All child
processes exited. This process run used the final enrollment implementation and
preceded only the final parent-deadline clock correction and its regression test;
that correction is covered by the final both-engine race gate. Process-source and
final-source hashes are recorded separately. This is not the fifteen-minute
partition acceptance.

## Limits and next work

The timer is not wired to health callbacks or a persisted incident/outbox. It does
not page. Config encoding still disables watchdogs, and the edge still rejects
an enabled unsupported watchdog config. No success stub replaces these guards.

Legacy session adoption waits for a live prior session to drain. An adopted probe
cannot use the unfenced acquisition method even after parent release. Run this
increment with a uniform worker version: old binaries predate the parent lease
and cannot honor its fence. Downgrade refuses to discard any persisted runtime
epoch, including a released one. No deployment or push was performed.

Subsequent work is accepted in [the watchdog record](M3_WATCHDOG_ACCEPTANCE.md): source
checkpoint/incident/delivery transactions, complete settings, application-health
callbacks, side-specific ownership and delivery reconciliation. Commands/offline
ACK, rotations/reset, cleanup, bounded flushing and the real 15-minute partition
are accepted in [the final M3 record](M3_COMPLETION_ACCEPTANCE.md).
