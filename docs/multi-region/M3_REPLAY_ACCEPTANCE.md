# M3 increment: ordered telemetry replay

2026-09-20, branch `codex/multi-region-probe-plan`, baseline `d3eea61`, implementation commit `f1095f8`.
This increment connects the existing autonomous availability runtime to durable
hub history and mirrors. M3 as a whole remains in progress. Nothing is pushed
or deployed by this work.

## Behavior and boundaries

The private edge store reads contiguous batches of exact persisted event bytes.
One session-owned loop sends at most one batch at a time through the existing
single writer. It handles readiness, retry delay and ACK input without blocking
ACK processing. Only an ACK covering the sent batch, under the exact durable
installation/probe/stream/generation fence, advances the local cursor and prunes
telemetry. Lost ACKs replay from the local cursor; welcome and health never prune.
A hub restored below already-pruned history blocks pending explicit recovery.

Hub ingestion uses a dedicated repository/service boundary; the older
observation-only foundation is not runtime authority. A serializable transaction
checks the enabled registration, enrolled stream, installation/key and current
DB-clock connector lease. It rechecks lease expiry before returning from the
write transaction. Existing bounded MariaDB deadlock retries cover conflicts
with configuration/assignment transactions.

`AccessService.AuthorizeEvent` decides from nonsecret facts captured by that
transaction: exact retained protected config, monitor-scoped membership at source
time, a seven-day history horizon, and exact accepted incident transition plus
linked channel/version for deliveries. Old generations write history only.
Future evidence cannot update live state. Incident identity comparisons use the
microsecond precision persisted by both adapters and the private edge store.

Migration 054 adds `probe_telemetry_receipts` to both hub engines. Accepted writes,
rejected receipts, dirty buckets and contiguous cursor progress commit together.
A full compact wire-event digest prevents a sequence from changing identity.
Duplicate accepted events do not rewrite mirrors or timestamps; duplicate rejected
events return their original reason. `duplicate_count` counts accepted duplicates;
`rejected` includes rejected duplicates, so counts partition the request. ACKs
prove only the sent prefix, even if the hub already holds a later prefix.

Replay never invokes providers, `HeartbeatService.Record`, escalation or legacy
alert creation. It does not create hub delivery intents. SQL/transport failures
close the session for idempotent reconnect; no successful ACK precedes commit.
The edge consumes `telemetry.retry`, but the hub does not yet emit retry frames
for recoverable storage failures; this protocol behavior remains unfinished.
There are no new dependencies, monitor types, notification providers or UI routes.

## Independent acceptance

Codex executed the checks; Antigravity authored files using file tools only.
All Go runs used `GOTOOLCHAIN=go1.26.6`; live MariaDB used the disposable database
`phoenix_m3_replay_ci` on `127.0.0.1:43316`. MariaDB was explicitly enabled, not skipped.

| Check | Evidence |
|---|---|
| Edge store contracts with race detection | PASS: bytes, count/byte bounds, holes, signed-64-bit exhaustion, ACK fences, both late-write rollback directions, concurrent append and reopen |
| Core authorization/service/connector tests | PASS: exact revision, monitor-scoped history, parent/channel authority, terminal outcome guards and trusted callback identity |
| Independent replay-loop regressions | PASS: old ACK does not immediately resend current batch; a queued durable ACK remains responsive during retry delay |
| Real TLS lost-ACK reconnect | PASS with race: higher welcome cursor retains local data, new generation replays exact bytes, duplicate ACK alone prunes |
| `TestProbeReplayAcceptance`, SQLite + MariaDB | PASS with race, 14.270s: mixed outcomes, late rollback, fences, retired history, future/live separation, exact authority, nanosecond identity, concurrent duplicates and lease expiry during a batch |
| Scoped `golangci-lint run` | PASS, 0 issues |
| CGO-free app, probe and admin builds | PASS; binaries under `/private/tmp/phoenix-m3-*` |
| Actual two-worker offline/restart/replay smoke | PASS, all 19 checks; `/private/tmp/phoenix-m3-replay-acceptance-1/report.json` |
| Complete repository tests with live MariaDB | PASS, 201.407s; `/private/tmp/phoenix-m3-mariadb-contracts.log` |
| `make gate-full` | PASS, exit 0: full Go race tests, zero lint issues, Svelte zero errors/warnings, frontend tests/build/lint, 12 Chromium journeys and Helm matrix; `/private/tmp/phoenix-m3-gate-full.log` |

The process test automatically applied config revisions 1 and 2, retained one
fenced management connection across two hub workers, then stopped all hubs.
A provider 503 left durable retry work; offline edge restart resumed it. Provider
recovery produced exactly one successful DOWN and one UP for the same incident.
The hub later committed the 22-event offline backlog through sequence 57,
including 17 exact observation sequences, one resolved incident and two final
sent outcomes. Hub-owned send intents remained zero. A second cold restart
continued to matching hub/edge cursors **69**, source high-water **72**, connection
generation **5**, and config revision **2**. A few newly created events can remain
queued while the running source advances; this is not a claim of a permanently
empty queue. The script stopped every process it started.

## Commit verification

Implementation commit `f1095f867d438677ead22e89341b77bf4a00124a` contains the 32 reviewed
source, test, migration and smoke-script files. Every committed file matched the
source manifest captured before staging; no source changed during final acceptance
documentation. Earlier local commits were preserved. Acceptance records and agent
lessons are committed separately. Nothing was pushed or deployed.

## Reproduction

Run the focused contracts with the test database environment above:

```sh
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -race -count=1 ./internal/adapters/repository -run '^TestProbeReplayAcceptance$'
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -race -count=1 ./internal/adapters/repository/edge ./internal/adapters/probe -run 'TestReplay|TestEdgeReplay|TestEdgeRuntime_RealTLS_Replay'
```

Build `./cmd/app`, `./cmd/probe` and `./cmd/phoenix-probe-admin` with `CGO_ENABLED=0`.
Use a fresh localhost MariaDB database ending in `_smoke` for `DB_DSN`, then run
`scripts/probe_runtime_smoke.py` with the three `--*-binary` paths, a new private
`--output` directory, `--verify-replay`, and `--mariadb-container` naming the local
container for independent read-only mirror/cursor queries.

## Review disposition and remaining work

The [work contract](M3_REPLAY_WORK_CONTRACT.md) and
[retrospective](M3_REPLAY_RETROSPECTIVE.md) record supervision and concrete fixes.
The initial broad Antigravity run was interrupted; neither its handoff nor its
`GOAL_COMPLETE` marker was treated as acceptance. All source ownership returned
to Codex before integration. No delayed agent source writer remains authorized.

This increment supports the three events produced by current M2 availability
execution: `observation`, `alert.transition`, `delivery.result`. Unsupported
auxiliary semantics receive explicit durable rejection; capabilities are not
expanded. Retention eviction and explicit gaps, state snapshots/recovery,
watchdogs, remote commands, aggregate/UI integration and fleet UI remain later
work. Existing telemetry/delivery capacity bounds remain; delivery history and
new hub receipts have no cleanup policy in this increment. Downgrade refuses
while receipts exist. These limits must stay visible in the next assignment.
