# Security and correctness audit — timezone/UTC discipline

Recorded 2026-09-24 against `b872106` on `codex/multi-region-probe-plan`.

This is a **verification record**, not a findings ledger. A prior audit session
reported one High and two Medium timezone findings plus a longer list (5 Medium,
4 Low, 2 Info). Four of those five items were re-checked line-by-line against the
committed tree and are **already remediated**. The longer list was never
recoverable — see [Lost scratch list](#lost-scratch-list). Nothing here is
inferred from the earlier report; every status below carries a command that was
actually run.

## Why the earlier report could not be applied as written

The audit was executed against a temporary snapshot (`.work/uptime-phoenix-audit/`)
that no longer exists, and its cited locations do not match this repository:

| Reported | Actual at `b872106` |
|---|---|
| `internal/adapters/http/handlers/heartbeats.go:70-78` | No such file. The handler is `heartbeat.go` (singular) and has existed under that name for the whole history of the file. |
| `durationSince()` in `monitor.go` and `status-page.go` | `durationSince` appears nowhere in `internal/`. `status-page.go` does not exist; the handler is `statuspage.go`. |
| `charts/uptime-phoenix/templates/deployment.yaml` | Exists, but the chart is split into `deployment-api.yaml`, `deployment-worker.yaml`, `deployment-web.yaml`, `deployment-extension.yaml`, `deployment-cloudflared.yaml`. A single-file edit would not have covered the pods. |

The conclusion is not "the audit was wrong about the bug" — the bug is real and is
written up in `AGENTS.md` rule 6. The conclusion is that it audited a snapshot that
predated the fix, so its remediation list was already stale on arrival.

## Finding status

### High — heartbeat chart window built from a local-zoned `now` — **already fixed**

The reported defect was that the chart range was built from `time.Now()`, which
carries the host zone, while heartbeats are written with `time.Now().UTC()`. The
driver renders a zoned value into SQL as its **local** wall-clock, so on a UTC+7
host the 1h/3h/6h ranges queried a window seven hours in the future and returned
zero rows forever. 24h "worked" because its window was wide enough to reach back
past the skew — the asymmetry that made this read as a frontend rendering glitch.

Current state:

- `heartbeat.go:184-188` — `heartbeatWindow()` returns `to = time.Now().UTC()`.
- Both callers go through it: `heartbeat.go:77` (list) and `heartbeat.go:116` (chart).
- `heartbeat_service.go:239` — the service re-normalizes on the way to the
  repository: `s.heartbeats.ListByMonitor(ctx, monitorID, from.UTC(), to.UTC())`.
  This is the "normalize at the service boundary so a careless caller can't
  reintroduce it" defense `AGENTS.md` rule 6 prescribes.
- The fix is not branch-local: `heartbeatWindow` was introduced by `5a0739e`
  (2026-07-28), which is an ancestor of `main`. Both the handler helper and the
  service boundary are therefore already in the released line.

### Medium — `durationSince()` compared wall-clocks in the host zone — **not present**

The function does not exist. The live equivalents are UTC at every step, and there
is no local-zoned `time.Now()` anywhere in non-test production code:

```
$ grep -rn "time\.Now()" --include=*.go internal/ | grep -v _test.go | grep -v "time.Now().UTC()" | wc -l
0
```

### Medium — status page parsed a wall-clock string as UTC — **not present**

`statuspage.go` contains zero `time.Now()` calls. Across all non-test handlers the
only `time.Now()` uses are already `.UTC()` (`apikey.go:63`, `badge.go:104`,
`feed.go:216`, `heartbeat.go:185`, `monitor_condition.go:81`), and
`feed.go:235` (`formatICalUTC`) is explicit about the zone on the wire.

### Related checks performed while verifying the above

These were not in the reported list; they are the neighbouring paths that could
carry the same defect class, checked so the record is not a partial scan.

- **Request-bound times.** `maintenance.go:196-197` and `:367-368` bind
  `start_date`/`end_date` as `time.Time` straight from JSON, which is the most
  likely place for an offset-bearing value to enter. They are normalized in the
  service, on both write paths: `normalizeAndValidateWindow` →
  `normalizeWindowDates` (`maintenance_service.go:54`, `:73-79`) forces
  `StartDate`/`EndDate` to UTC, and both `Create` (`:46`) and `Update` (`:126`)
  call it. `IsActive` reads with `time.Now().UTC()` (`:216`) and compares
  `mw.StartDate.UTC()` (`:223`).
- **Location-aware cron is intentional and correct.** `maintenance_service.go:239`
  (`locationForTimezone`) and `cron_eval.go:36` (`nowLocal := now.In(loc)`) evaluate
  in the window's IANA zone so `0 2 * * *` means 02:00 Bangkok, not 02:00 UTC. This
  is not the bug class: `In(loc)` preserves the instant, and both sides of the
  activation comparison live in that same location (`cron_eval.go:54`). `nil` loc
  falls back to UTC (`cron_eval.go:26-27`) rather than panicking. Empty or invalid
  stored zones degrade to UTC (`maintenance_service.go:239-249`) and are rejected at
  write time.
- **No unlocated parses.** Zero `time.Parse`/`time.ParseInLocation` and zero bare
  `time.Date(` in non-test `internal/`; no DSN carries a `loc=` parameter that
  could disagree with the Go side.
- **Chart labels cross the wire as absolute instants.** `heartbeat.go:137` formats
  buckets with `time.RFC3339`, and `ResponseTimeChart.svelte:75` parses them with
  `new Date(b.time)`, so d3 renders axis ticks in the viewer's own zone. This is
  the behaviour the report asked for; it is already how the code works. A zone-less
  label here would make JS parse the string as *viewer-local* and shift the axis by
  the viewer's offset — the client-side mirror image of the server bug — which is
  why the new test asserts the designator is present.
- **`TZ` in the Helm chart is deliberately absent.** There is no `TZ` anywhere under
  `charts/`. Given the above, injecting one would be the wrong fix: the invariant
  is "wall-clock is UTC across the DB boundary, host zone is irrelevant", not "make
  the host zone match the data". A container `TZ` would silently re-introduce the
  skew the moment any caller forgot `.UTC()`, because it would make the
  un-normalized path look correct in production and wrong in tests.

## Gaps this pass did find

Both are test-coverage gaps, not product defects. They matter because the audit
class in question is precisely the one where a passing suite means nothing.

1. **`GetChartData` had no regression coverage at all.** The reported symptom was a
   blank *chart*, but `heartbeat_window_test.go` registered the `/chart` route and
   then only ever requested the list route — `grep -c "heartbeats/chart"` on the
   test file returned 0. The chart endpoint shares `heartbeatWindow()` with the list
   path, so the fix works, but "the same helper is used" is a reading, not a test.
   Added `chart_hours_{1,3,6,24}` subtests asserting the window is non-empty, the
   bounds reach the repo in UTC, and the bucket label carries a zone designator.

2. **The service-boundary normalization was untested on the read path.**
   `DeleteOlderThan` had `TestHeartbeatService_DeleteOlderThan_CutoffIsUTC`;
   `ListByMonitor` — the load-bearing `.UTC()` at `heartbeat_service.go:239` — had
   no equivalent. Added `TestHeartbeatService_ListByMonitor_BoundsAreUTC`.

### Why the handler test alone cannot guard this

The first version of this record claimed the existing handler test covered the
fix. It does not, and that was only established by breaking the code on purpose:

```
$ perl -pi -e 's/to = time\.Now\(\)\.UTC\(\)/to = time.Now()/' .../heartbeat.go
$ go test ./internal/adapters/http/handlers/ -run TestHeartbeatHandlers_ShortRanges
ok      github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers   0.555s
```

The test **passed with the bug restored**, because the service boundary
re-normalizes the bound before the fake ever sees it. Defense in depth makes the
outer test blind. This is `AGENTS.md` rule 6's own stated trap from the other
direction: an in-memory fake compares instants, and instants are zone-independent,
so no row-count assertion at the handler level can observe the zone at all. The
only assertion that carries information is the `Location()` of the bound the
repository receives — hence gap 2 above, which does fail under the same mutation:

```
--- FAIL: TestHeartbeatService_ListByMonitor_BoundsAreUTC (0.00s)
    from bound Location() = UTC+7; want time.UTC — a local-zoned bound is rendered
    into SQL as its local wall-clock, shifting the window by the host offset
```

The new chart-label assertion was mutation-checked the same way. Replacing
`time.RFC3339` with `"2006-01-02T15:04:05"` fails `chart_hours_1` and `chart_hours_3`
with "carries no zone designator". Both new assertions were verified to fail on the
defect they claim to guard, and both files were restored afterwards (`git diff`
shows test-only additions).

## Real-engine verification and a corrected code comment

The first draft of this record stated honestly that no real database had been
exercised. That gap is now closed, and closing it produced a finding.

### The mechanism, measured on MariaDB 11.8.9

Against a live InnoDB `TIMESTAMP` column, with `loc=UTC` present in the DSN, the
same instant passed as a UTC value and as a UTC+7 value is written differently:

```
go-value=2026-03-14T06:30:00Z     stored=2026-03-14 06:30:00
go-value=2026-03-14T13:30:00+07:00  stored=2026-03-14 13:30:00
```

And on the query path production actually uses (bun `Where("time >= ?", bound)`):

```
UTC bounds        -> time >= '2026-09-24 03:30:55'   count=1
local(+7) bounds  -> time >= '2026-09-24 10:30:55'   count=0
```

Identical instants; the bound's *location* alone decides whether the row exists.
AGENTS.md rule 6's mechanism is therefore confirmed on the engine, not merely
asserted from the Go side, and the in-memory fake's blindness is confirmed too.

### Finding: two adapter comments asserted a false driver claim

`mariadb.HeartbeatRepo.ListByMonitor` justified its own `from.UTC()` / `to.UTC()`
as "belt-and-braces", on the grounds that "the MySQL driver does convert to the
DSN's `loc=UTC`". The measurement above shows that is false on both the raw
parameter path and the bun path. `loc=UTC` governs how stored values are parsed
*back into Go*; it does not rewrite a bound being written out.

This is not cosmetic. A comment that says a guard is redundant is an invitation to
someone later to delete it, and deleting it reintroduces the blank-chart bug on the
one engine where the comment claims it cannot exist.

The mirror-image claim sat in `sqlite.HeartbeatRepo.ListByMonitor`, which asserted
that SQLite renders a zoned bound while "unlike the MySQL driver, which converts
to the DSN's `loc=UTC`". Both halves are wrong or unsupported: MySQL shifts, and
SQLite did *not* shift in the same measurement. Only the MySQL half is corrected
with evidence; why the SQLite path tolerates a zoned bound was not investigated,
so the rewritten comment states the measurement and explicitly declines to assert a
mechanism. The `.UTC()` calls themselves are unchanged on both engines — they are
correct either way, and keeping both adapters identical is deliberate.

### Durable coverage added

`internal/adapters/repository/heartbeat_utc_matrix_test.go`:

- `TestHeartbeatUTCBoundContract_{SQLite,MariaDB}` — a UTC-normalized one-hour
  window finds a 30-minute-old heartbeat on both engines.
- `TestHeartbeatUTCBound_MariaDB_LocalZonedBoundShiftsSQL` — the three-part
  statement: a raw bun query with a local-zoned bound loses the row (hazard),
  `loc=UTC` does not save it, and the adapter's normalization returns it (defense).
  It asserts the *premise* too, that both windows denote identical instants, so the
  comparison cannot quietly become meaningless.

Mutation-verified on the live engine: deleting `.UTC()` from
`mariadb.HeartbeatRepo.ListByMonitor` fails it with "adapter returned 0 rows for a
local-zoned bound". The MariaDB-only scoping is deliberate — asserting the shift on
SQLite would encode semantics this measurement did not confirm.

## Lost scratch list

The remaining 11 findings (5 Medium, 4 Low, 2 Info) from the prior audit are
**not recorded here, because they could not be recovered.** They lived only in the
deleted `.work/` scratch directory and in the compacted portion of the previous
session transcript. Searched and came up empty:

- `find` for `SECURITY_AUDIT*` across the repo, and `git log --all --diff-filter=A`
  for the same path — never committed on any branch.
- `git stash list` (empty), `git reflog -15`, `git worktree list`.
- All four worktrees: `~/.codex/worktrees/4f9b/`, both
  `~/.gemini/antigravity/worktrees/` copies.
- Every session transcript under
  `~/.pi/agent/sessions/--Users-fizto-Desktop-Work-Hobby-uptime-phoenix--/`. The
  2026-09-23 session (2.5 MB) contains zero occurrences of `Severity`, `High`,
  `Medium`, `heartbeats.go`, `durationSince`, or `uptime-phoenix-audit`. The only
  file matching `uptime-phoenix-audit` is the current session's own log.
- `~/.codex/sessions/`, `/tmp`, `/var/folders`.

Reconstructing them would have meant inventing findings, so this document records
what could be verified instead. The whole-branch surface that was never audited is
large — 76 commits and 467 Go files, ~118k insertions over the merge-base with
`main` (`5183093`) — so treat this as covering the timezone/UTC class only.

## Verification evidence

Distinguishing authored from executed, per `AGENTS.md` rule 13.

| Gate | Result |
|---|---|
| `go build ./...` | pass |
| `gofmt -l internal/` | empty |
| `go vet ./internal/adapters/http/... ./internal/core/services/...` | pass |
| `GOTOOLCHAIN=go1.26.6 golangci-lint run` on all touched packages | **0 issues in touched files**; 5 pre-existing reports elsewhere |
| `go test -count=1 -run 'Heartbeat\|Maintenance\|Timezone\|UTC\|Window\|Cron\|Escalation\|IsActive\|Insight'` over handlers + services + scheduler | **152 named passes, 0 fails, 0 skips** |
| `go test -race -count=1 ./internal/core/services/ ./internal/adapters/http/handlers/` | **919 passed** |
| Mutation check, service `.UTC()` removed | new test fails (correctly) |
| Mutation check, handler `.UTC()` removed | handler test passes — documented above as a known-blind layer |
| Mutation check, chart label made zone-less | new chart subtests fail (correctly) |
| **Live MariaDB 11.8.9** (`phoenix_ci`, `TEST_MARIADB_DSN` set) | `TestHeartbeatUTCBound*` **3 named passes, 0 skips** |
| Mutation check on live MariaDB, adapter `.UTC()` removed | fails with "adapter returned 0 rows for a local-zoned bound" |
| Repo-wide gate, race detector + live MariaDB: `go test -race -count=1 -timeout 2400s -p 4 ./...` | **22 packages ok, 0 FAIL, 0 data races, 0 panics** (slowest package 806s) |
| Same gate scoped to the MariaDB contracts: `-race -run 'TestRepositoryContract_MariaDB\|TestHeartbeatUTCBound\|TestEscalationContract_MariaDB'` | **26 named passes, 0 fails, 0 skips** (25 MariaDB-family) |

The `-race ./...` log was captured without per-test detail, so on its own it could
not distinguish an executed MariaDB contract from a silently skipped one — the
failure mode `AGENTS.md` rule 13 warns about. The final scoped row is the evidence
that the MariaDB paths genuinely run under the race detector, not merely that the
package reported `ok`.

### Not verified

- `golangci-lint run` now **has** been run, at CI's pinned v2.12.2. It initially
  appeared broken — it panicked inside `go/types` while parsing the standard
  library — but that was a toolchain mismatch, not a code defect: the binary is
  built with Go 1.26.5, the machine default is 1.27.1, and `go.mod` declares
  `go 1.26.6`. Run it as `GOTOOLCHAIN=go1.26.6 golangci-lint run` and it works.
  The same trap is now documented in `docs/TESTING.md` §4.1, because misreading it
  as "the linter is broken here" leads straight to skipping a required gate.
  **Zero issues in any file this pass touched.** The 5 remaining reports
  (`1 unconvert` in `condition_mirror.go`, `4 unparam` in `probe_capacity_*_test.go`)
  are pre-existing debt in files untouched since the capacity slices; the gate is
  "zero warnings on new code", so they are left alone deliberately.
- Why SQLite tolerates a local-zoned bound was not investigated. It is reported
  as measured behaviour only, and the rewritten adapter comment says so. The
  engine-independent consequence — that both adapters keep the normalization — is
  verified; the reason the SQLite path is forgiving is not.
- The frontend gates (`bun run check`, `bun run build`) were not run. Nothing in
  `web/` was modified; `ResponseTimeChart.svelte` was read only.
- The Low/Info findings, and any finding outside the timezone class, remain unaudited.

## Files changed by this pass

- `internal/adapters/http/handlers/heartbeat_window_test.go` — chart-route subtests.
- `internal/core/services/heartbeat_service_test.go` — fake records its query
  bounds; `TestHeartbeatService_ListByMonitor_BoundsAreUTC`.
- `internal/adapters/repository/heartbeat_utc_matrix_test.go` — new; the two-engine
  UTC window contract plus the live-MariaDB mechanism test.
- `internal/adapters/repository/mariadb/repo.go` — comment only: corrected the
  "belt-and-braces / driver converts to loc=UTC" claim.
- `internal/adapters/repository/sqlite/repo.go` — comment only: corrected the
  mirror-image claim; states the measurement, declines to assert an unverified
  mechanism.

No executable production code was modified: the timezone fixes were already in the
tree, and the adapter `.UTC()` calls were verified correct and left in place.
