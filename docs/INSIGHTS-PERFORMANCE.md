# Insights performance — issue #37

[Proposal](https://github.com/fiztoz/uptime-phoenix/issues/37): reduce Insights
latency without changing its reliability, coverage or visibility rules.

## Review and implementation scope

The existing code already batches heartbeat reads and uses indexed leading-state
lookups and rollups. The first useful change is to overlap the three independent
reads, reduce the selected monitor set before loading it, and reuse calculations.
A dedicated transition table adds persistence and retention obligations before
we have evidence it is needed; it remains a profiling-driven follow-up.

Implemented:

- Prometheus stage histograms and cache outcome counters, through a narrow port.
- Concurrent transition, leading-state and aggregate reads; first failure cancels
  siblings and preserves the original error. All reads use the same UTC bounds.
- Descendant-group filtering in both SQL adapters, intersected with the monitor
  allowlist and type filter. Empty restricted sets always return zero rows.
- A process-local 20-second cache capped at 64 entries and 50,000 retained rows.
  Entries larger than the row cap are computed and coalesced but not retained.
- Request coalescing with independent waiter cancellation and retry if a leader
  is canceled. No failed response is cached and no detached calculation survives
  its owning request.
- Fresh authorization and monitor selection before every cache lookup. The key
  includes user, period, type, selected group and the selected monitors' relevant
  metadata. This preserves access revocation and discovery of new monitors.
- Metric-independent cached data, deep-copied and sorted per caller. The HTTP
  response shape and frontend metric-tab behavior are unchanged.

The original user/period/type/group cache key was insufficient on its own:
revoking a grant could otherwise leave that monitor visible until expiry. The
new key reflects the actual authorized monitor set. Cached responses keep their
original `from`/`to`, making their data window explicit rather than implying a
fresh calculation.

There are no new dependencies or schema changes. The cache is independent per
API replica. SQLite's pool still has one connection, so overlapping service
reads do not imply parallel SQLite SQL execution. MariaDB pool contention can
reduce the benefit; stage timing includes that wait.

## Correctness validation

`insights_performance_test.go` verifies actual overlap, each read failing and
canceling siblings, error retry, UTC normalization, sequential/concurrent result
equality for 24h/7d/30d/90d, cache expiry, response mutation isolation, filter/user
isolation, partial and complete grant revocation, monitor addition/deletion/edit,
group moves, coalescing, leader/waiter cancellation and retention limits.

`insights_filter_test.go` runs the same filter and service contract against real
SQLite and MariaDB. It covers nested-group selection, intersections with empty
and nonempty allowlists, existing single-group/ungrouped filters, and revocation
after a cached service call. The repository race suite passed against both
engines, using an isolated MariaDB 11.4 database for validation.

The existing reliability calculation and boundary-bucket tests remain unchanged.

`rtk proxy make gate-full` passed: Go build/vet/race tests, zero lint issues,
Svelte checks (zero errors/warnings), frontend tests/build/lint, all 11 Playwright
tests, Helm render/validation matrix and no reachable vulnerabilities reported
by `govulncheck`. The final additional filter/revocation tests also passed with
the race detector. The temporary MariaDB test container was removed afterward.

## Controlled benchmark

Run from the repository root:

```sh
rtk proxy env GOTOOLCHAIN=go1.26.6 go test ./internal/core/services \
  -run '^$' -bench '^BenchmarkInsightsReadPath$' -benchtime=100ms -count=3
```

This benchmark compares the sequential calculation with concurrent cold and
cached calculations, including cache-key construction, response copying and
sorting. Each mock repository read has a controlled 2 ms delay. The fixture
contains sparse transitions and rollups, **not** 90 days of dense rollup rows.
It excludes authorization, monitor SQL, HTTP serialization and network transit.
These are service microbenchmarks, not production `/api/insights` latency claims.

Initial medians in milliseconds, three samples on Apple M5 / darwin arm64:

| Monitors | Period | Sequential | Concurrent cold | Cached |
| ---: | :--- | ---: | ---: | ---: |
| 100 | 24h | 6.819 | 2.358 | 0.036 |
| 100 | 7d | 6.795 | 2.351 | 0.034 |
| 100 | 30d | 6.830 | 2.392 | 0.033 |
| 100 | 90d | 6.882 | 2.392 | 0.033 |
| 1,000 | 24h | 7.439 | 3.238 | 0.334 |
| 1,000 | 7d | 7.660 | 3.701 | 0.327 |
| 1,000 | 30d | 7.658 | 3.639 | 0.327 |
| 1,000 | 90d | 7.706 | 3.581 | 0.324 |
| 5,000 | 24h | 10.195 | 6.185 | 1.792 |
| 5,000 | 7d | 10.064 | 6.132 | 1.800 |
| 5,000 | 30d | 10.003 | 7.932 | 1.760 |
| 5,000 | 90d | 9.968 | 6.150 | 1.784 |

The measurements demonstrate removed serialized read delay and avoided work on
hits. Cold requests allocate more for the fingerprint and independent response;
for 5,000 monitors/24h, approximately 6.89 MB versus 5.92 MB in the sequential
reference, while cache hits allocate approximately 1.23 MB. These timings are
illustrative local measurements and vary with competing host activity.

## Remaining profiling before schema optimization

Issue #37's full production-scale acceptance matrix remains a follow-up. Collect
actual API and stage durations for 100/1,000/5,000 monitors over 24h/7d/30d/90d,
with realistic rollup density and historical partitions. Include cold/repeated,
group-filtered, admin/all-visible and restricted-user requests, plus simultaneous
distinct users to expose connection-pool pressure.

On representative MariaDB data, inspect the existing leading lookup with
`EXPLAIN PARTITIONS`, preserving the `time DESC, id DESC` tie-break. Do not impose
a historical lower bound without an unbounded fallback: an old stable monitor
can legitimately have no recent important transition.

Only then decide whether SQL summaries of complete daily buckets or dedicated
transition persistence justify their complexity. Partial-bucket coverage,
latency sample counts, retention and equivalent behavior on SQLite must remain
part of that design.
