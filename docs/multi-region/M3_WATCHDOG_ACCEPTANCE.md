# Enabled connection watchdog acceptance

Base commit: `8e7b85b`. This increment completes watchdog mirror authorization and
enables the already-integrated source timer and provider paths. Both hub and edge
watchdogs now operate from saved settings and exact applied configurations. The
remaining M3 command, rotation/reset, shutdown and long-partition requirements are
not covered by this acceptance.

## Implemented behavior

Enabled remote snapshots require explicit probe name/location metadata and the
`watchdog.v1` capability. Capability derivation, transfer validation and the real
edge inventory agree; an older edge cannot accept a watchdog-enabled snapshot.
Local-only operation still defaults to disabled settings. Saving settings creates
desired work; the separate exact revision/hash receipt proves durable application.

The replay DTO mapper now retains probe watchdog incidents with null monitor and
assignment generation. `AccessService.AuthorizeEvent` is the authorization choke
point. It checks retained exact protected config, source lifecycle identity and
versions, and watchdog channel membership. Delivery notification versions equal
snapshot revisions in V1. A resend may therefore use config 3 while retaining a
transition opened under config 2; its parent identity never changes. Administrative
disable may resolve an existing outage under a disabled graph, without generating
recovery provider work. A disabled graph cannot open another incident.

The durable hub source registry fences every retained hub watchdog UUID, including
resolved historical incidents. Forged edge transitions and outcomes receive
per-sequence rejection receipts and cannot overwrite hub-owned history. Accepted
edge mirrors create neither hub delivery intents nor hub source journal events.
Watchdog parent receipts preserve null monitor/generation and reconstruct the
exact transition kind. Sequence, lifecycle version and delivery attempt order
watchdog history across wall-clock rollback; horizon and future-evidence bounds
still apply. Duplicate receipts verify the original digest/kind without rewriting
mirrors. A late cursor failure rolls back the whole batch.

Remote ACK metadata remains rejected with `acknowledgement_unauthorized` until the
durable command implementation can correlate the operator's actual command. An
authenticated edge plus an actor name is not sufficient authority. This increment
does not introduce a fabricated assignment generation or pretend ACK commands work.

## Real process evidence

`scripts/probe_runtime_smoke.py --verify-replay --verify-history --verify-watchdog`
passed all 33 stages using current app/probe/admin binaries, two hub workers,
private edge SQLite and a fresh disposable MariaDB schema. A transparent local TCP
relay interrupts only the management connection; TLS remains end to end and both
source processes keep running. All target/provider traffic stays local.

The earlier 25 stages still pass: enrollment with active workers, saved config
updates, normal availability DOWN/retry/UP while offline, edge restart, ordered
replay and production history recomputation. The new stages then apply enabled
watchdog config revision 3, establish healthy application frames, sever the link,
and observe two distinct source-owned DOWN notifications in 92.04 seconds.
An edge restart while partitioned preserves the same firing incident and does not
repeat its initial notification. After the relay restores traffic, both sides send
UP after 64.09 seconds, including reconnect/backoff and the configured 30-second
continuous-health recovery period. These are measured process times, not a tighter
timer guarantee.

The local sink receives watchdog statuses `[DOWN, DOWN, UP, UP]`, one pair for each
source UUID. Edge watchdog history reaches the hub at resolved version 2 with two
sent provider outcomes, zero replay rejections, and zero hub send intents for the
edge incident. Final source and acknowledged sequence are both 200. The harness
joins/stops every spawned process and closes the local servers on success/failure.
This partition is deliberately shorter than the still-required M3 fifteen-minute
scenario with a queued ACK and rotation/reset dependencies.

## Review and learning record

Antigravity performed a no-tools, read-only design and implementation review in
conversation `6021bb6b-40c0-4daf-95f6-31c24e785d82`; it owned no files. Its initial
claim that notification versions were independent entity versions contradicted
`config_references.go` and protocol V1. The reviewer withdrew it after receiving
the enforced equality and existing resend tests. It also withdrew the suggestion
to accept uncorrelated ACKs before command support exists. Baseline seams already
listed in the work contract were classified as requirements, not new defects.

Checking hub ownership for delivery outcomes as well as transitions was retained
as an explicit defense and regression. The previous missing-parent check already
blocked the claimed ordinary delivery stall, so this is not recorded as a fixed
production bug. The final implementation review found no actionable issues; it
did not execute tests and assumed nullable receipt columns. Real engine assertions
verify those nulls. Review confidence remains bounded by its supplied files.

The coding lessons are to read the actual version contract, distinguish current
send eligibility from historical mirror authorization, correlate operator actions,
and preserve source ordering without manufacturing monitor context. See the
[delivery retrospective](M3_WATCHDOG_DELIVERY_RETROSPECTIVE.md) for the separately
reproduced lease-clock and provider defects fixed in the preceding commit.

## Final gate

The full Go race gate passed in 22 tested packages with 3,288 named pass events,
241 MariaDB-named pass events and 204 lowercase `/mariadb` engine subtest events.
There were no failures or MariaDB skips. Counts include parents and some SQLite
tests named for MariaDB regressions; they do not count independent scenarios.
The two existing named skips are the opt-in real MongoDB checker and the legacy
Telegram hardcoded-URL test. Thirteen packages have no tests. Current redirected
Telegram watchdog request tests passed as part of the notifier package.

CGO-free build, zero-issue lint and diff whitespace checks passed. All 16 changed
Go/script hashes match both full test and process runs. No framework/driver imports
entered core and no schema, dependency, frontend or Helm change is included. See
[immutable source/log evidence](M3_WATCHDOG_EVIDENCE.json).

```text
Commands executed: go test -race -count=1 -timeout=20m -json ./...;
  CGO_ENABLED=0 go build ./...; golangci-lint run; git diff --check;
  scripts/probe_runtime_smoke.py --verify-replay --verify-history --verify-watchdog.
Engines and named tests exercised: real SQLite and MariaDB;
  TestWatchdogReplayStorage/{sqlite,mariadb}/{ResendHistoryAndClockRollback,
  HubSourceOwnership,AtomicRollback,AdministrativeDisable};
  TestWatchdogReplayLifecycleAuthority; TestWatchdogReplayDeliveryUsesResendConfigAndSourceOrder;
  TestReplayWatchdogDTOUsesAuthenticatedProbeAndNoMonitor;
  TestEnabledWatchdogRequiresMetadataAndCapability; legacy full-suite contracts;
  real hub/probe/admin processes, two competing workers, local TCP partition and sink.
Passed / failed / skipped: 22 tested packages and 33 process stages passed;
  0 failures and 0 MariaDB skips; 2 existing named skips; 13 no-test packages;
  build passed and lint: 0 issues.
Acceptance criteria still unverified: durable commands/offline incident ACK,
  credential/certificate rotation, explicit stream reset, bounded flush and the
  real fifteen-minute partition; final whole-M3 frontend/process gate.
```

Continue [the durable command contract](M3_COMMAND_WORK_CONTRACT.md), then the
remaining [M3 completion requirements](M3_COMPLETION_WORK_CONTRACT.md). This is
watchdog acceptance, not whole-milestone completion.
