# M3 historical replay and coverage acceptance

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Status: historical recomputation increment accepted after independent integration.
Branch: `codex/multi-region-probe-plan`, following `a1adb01`.
Codex owns implementation and validation. Nothing is pushed or deployed.

## Implemented behavior

Remote and local source commits queue original-time regional 1m/1h/1d and overall
work. The production worker runs in `all` and `worker` modes, resumes durable work
on startup and joins shutdown before the database closes. No provider work occurs
inside projection. Closed windows are processed in bounded batches.

Migration 057 adds duration coverage, managed-rollup markers, fresh dirty-work
revision tokens, historical seed indexes and durable backward-clock repair ranges.
Regional counts/latency remain statistics of actual samples. Gaps contribute
UNKNOWN time without inventing samples. Hour/day coverage merges at most 60/24
children; older legacy children retain their sample statistics without inventing
known duration. Legacy rollup writers cannot overwrite managed coverage.

The same coherent bounded evidence reader supplies background and on-demand
history: actual assignment generations/policies, sequence-leading seeds before
freshness lookback, retained samples and declared loss. Assignment changes queue
removed and added members even if no subsequent check arrives. Outside membership
there is no regional duration denominator; overall missing evidence stays UNKNOWN.

MariaDB uses repeatable-read evidence, a fresh random dirty revision, a current
locking read and final revision-checked consume. SQLite obtains its writer lock
before reading. Parent dependencies are checked inside the same snapshot as the
token. Competing source writes either serialize or leave work queued. MariaDB
serialization/deadlock errors roll back and retry on a fresh batch.

A late higher sequence with a backward source clock can invalidate timestamps far
beyond its freshness interval. The source transaction merges a durable repair
range; the worker expands it one minute at a time through the affected evidence's
freshness end, re-marking every parent and overall minute. Retention gap expansion
also advances a durable cursor in bounded steps. Monitor deletion cascades dirty
work and repair ranges; gap expansion skips deleted monitors.

## Verified focused cases

The JSON run `phoenix-m3-history-range-contracts.jsonl` executed 32 tests with zero
failures and zero skips, including real MariaDB, before the final read/legacy
corrections. Those corrections subsequently passed their original reproductions
and the complete final real-engine matrix described below.

- Replay and gaps three days outside the usual lookback update minute/hour/day
  coverage and overall intervals; duplicate replay creates no new work.
- Gap boundaries, zero-time gaps and forward-skewed older sequences cannot
  fabricate coverage. Newer retained sequences can restore continuity.
- Last-write failure rolls back all projections and leaves work for a new service.
- Consume/reinsert ABA cannot erase a new random revision.
- A concurrent MariaDB source commit cannot publish a stale projection; independent
  SQLite connections serialize source re-marking behind the projection writer.
- Hour/day selection before a concurrent source commit cannot consume a fresh
  parent token before the dirty child finishes.
- Removed/new membership contributes the exact appropriate fractional minute.
- A late clock regression repairs an already-computed bucket an hour later.
- On-demand history agrees with sequence-aware background history.
- Legacy sample counts and weighted latency survive parent recomputation.
- Replacing a middle overall interval preserves both adjacent fragments.
- The downgrade guard refuses to discard managed duration coverage or repair work.

## Production process evidence

The extended `scripts/probe_runtime_smoke.py --verify-replay --verify-history`
passed with fresh disposable MariaDB, two hub workers, an actual probe process,
local target and local provider sink. Report:
`/private/tmp/phoenix-m3-history-process-acceptance/report.json`.

The run retained 21 offline events, replayed all 16 retained observation sequences,
mirrored one resolved incident and two successful delivery outcomes, and created
zero hub send intents. A cold restart resumed beyond acknowledged backlog. The
production history worker recomputed a closed minute with an exact matching
source count and 60,000,000 microseconds of overall coverage; no minute/overall
work remained for that window. Final source/edge committed cursors were both 48.
All spawned processes shut down successfully. This is a short process acceptance,
not the required final 15-minute M3 partition test.

## Final gate ledger

The full Go race suite passed (handler package 360.373s; main repository 333.793s).
After the final shared-reader/legacy-statistics corrections, the complete database
matrix passed again (main repository 326.296s; SQLite 63.377s; edge 7.761s;
MariaDB adapter 1.465s), with 193 MariaDB-named pass events and zero MariaDB skips.
Final core race tests passed (services 16.499s), CGO-free `go build ./...` passed,
lint reported zero issues, and source-manifest/core-boundary/whitespace checks
passed. No frontend, Helm or dependencies changed; their broader gates remain
part of final whole-M3 acceptance. Exact hashes and critical tests are in
[M3_HISTORY_DB_EVIDENCE.json](M3_HISTORY_DB_EVIDENCE.json).

The prior broad run exposed out-of-order migration test fixtures: 037 rebuilt
SQLite rollups and 035's restore list omitted 057. Fixtures now remove/restore 057
in schema order. No acceptance is inferred from that failed run or skipped tests.
Use `TEST_MARIADB_DSN`, never `MARIADB_TEST_DSN`.

## Boundaries and remaining milestone work

This increment supplies historical projection for the supported availability
runtime. It does not expose the later fleet/history UI or add condition/TLS edge
support. Existing administrative pause semantics remain; reconstructing a separate
historical pause timeline belongs to compatibility work. Reads fail explicitly
above 100,000 observations or 10,000 assignment/seed/gap records per evidence
window; they do not return partial success. Receipt/history retention and physical
disk cleanup remain separate M3 work.

Both watchdogs, durable commands and offline ACK, rotations/reset, cleanup, bounded
shutdown flush and the real 15-minute partition acceptance remain open. M3 is not
complete. See [retrospective](../postmortems/2026-09-21-m3-integration.md#replay-and-history) for verified failures,
Antigravity's contribution and the integration lessons.
