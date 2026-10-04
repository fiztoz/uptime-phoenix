# M4 Insights and navigation cache invalidation

Insights and the dashboard navigation caches now follow overall projection
versions. A monitor that already has overall history is ranked from that
history instead of pooled heartbeats. This is the Insights bullet of
[M4](IMPLEMENTATION_PLAN.md). Backup/restore and deployment documentation
remain open.

## Behavior

The Insights cache key now includes each visible monitor's stored
`projection_version`. The version read is one query per 500 ids, and it runs
before the cache lookup. A version change misses the cache. The same version
still hits. Installs with no projection reader keep the previous key and the
three batched heartbeat reads.

On a cache miss, overall history for the same monitors is one more batched
read, overlapped with the existing transition, leading-state, and rollup
reads. A monitor with no history row keeps the heartbeat ranking. A monitor
with history uses clipped policy durations for availability, downtime,
coverage, outages, and flaps. Time the history does not cover is unknown.
An outage that was already open at the start of the window is not counted
again. Flaps are UP/DOWN changes inside the existing 24-hour flap window.
Latency is left empty for those rows so several probes are not averaged into
one ping. The response adds `projection_version`, the highest version in the
authorized set. Zero means none of those monitors has a projection.

The browser heartbeat and status-change events carry `projection_version`
when the check belongs to a remote assignment. An older version does not move
the monitor badge. A newer one clears the folder catalog and the dashboard
Insights preview once per animation frame. It does not request the monitor
list. Disconnecting clears the remembered versions and invalidates those
caches again so the next connection can resynchronize.

## Verification

Executed in this checkout:

- `gofmt -w` on the changed Go files.
- `go test -count=1 -timeout 180s` with `TestInsights`, `TestApplyOverall`,
  `TestMarshalWireEvent_Heartbeat`, `TestMonitorHealth`,
  `TestGroupAlert_Overall`, and `TestStatusForMonitors` across services,
  websocket, HTTP handlers, bootstrap, and repository — 44 passed. The
  repository package compiled; this filter did not execute a new MariaDB case.
- `go test ./internal/core/services -run '^$' -bench '^BenchmarkInsightsReadPath$' -benchtime=50ms -count=1`
  — passed. Cached runs stayed far below the cold runs. This is a
  microbenchmark, not a production latency claim.
- `cd web && bun test src/lib/projection-version.test.ts src/lib/monitor-filter.test.ts src/lib/status-page-health.test.ts`
  — 62 passed.
- `cd web && bun run check` — zero errors.

Not run: MariaDB for the new version and history queries, `golangci-lint`,
`bun run build`, or a browser pass. Local acceptance does not authorize push,
deployment, or a production migration.
