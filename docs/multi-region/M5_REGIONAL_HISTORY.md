# M5 regional history and chart routes

> Historical slice record. Remaining M5 work was completed on 2026-09-29; see
> [M5 completion and current acceptance](M5_COMPLETION.md).

Status: fifth M5 slice (part 1 of "Regional history and UI"), built on the
[read API foundation](M5_FOUNDATION.md), [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md),
[assignment writes](M5_ASSIGNMENT_WRITES.md) and
[administrative operations](M5_ADMIN_OPERATIONS.md). It implements the
**relationship-checked regional history/chart routes**. Part 2 (the UI half
with latency selection and the section-7.2 compatibility activation) is
recorded below; the fleet/monitor views and their browser coverage remain.

## What is implemented

| Endpoint | Behavior |
|---|---|
| `GET /api/monitors/:id/probes/:probe_id/heartbeats` | Relationship-checked regional history rows; preserves the existing `hours`, `limit`, `order`, `important` query semantics |
| `GET /api/monitors/:id/probes/:probe_id/heartbeats/chart` | Regional chart with latency buckets from this probe's measured samples plus downtime/unknown intervals |

Both routes use the existing session JWT middleware and monitor visibility
through `AccessService`; missing and hidden monitors return the same 404.
`PROBES_ENABLED=false` returns the typed 503 after authentication, exactly
like the other M5 reads. Successful reads are `no-store`.

## Relationship check (the point of the slice)

Before any evidence is read, the monitor/probe relationship is validated:
the probe must be in the monitor's **current desired assignment set** or have a
**recorded assignment interval overlapping the requested window**. A probe
that never belonged to this monitor reads exactly like a missing one —
`404 probe_not_found` — and no evidence store is touched for it. Unassigning a
probe does not erase access to the evidence it produced while assigned
(historical attribution is preserved); the engine test asserts both directions
on both engines.

Errors retain `error` plus `code`: `invalid_monitor_id` and `invalid_probe_id`
(400), `monitor_not_found` and `probe_not_found` (404),
`probes_disabled` and `regional_unavailable` (503). Storage failures are
redacted.

## Wire contract

Regional history rows keep the existing heartbeat wire names (`id`,
`monitor_id`, `status`, `ping`, `message`, `time`, `important`) and add
`probe_id`, `received_at`, `assignment_generation` and `config_revision`
exactly as the frozen M0 shape prescribes. Generations/revisions are decimal
strings (the fixtures exceed `2^53 - 1`), `message` stays `message`, and the
status vocabulary is the lowercase regional set `up`, `down`, `pending`,
`maintenance`, `unknown` — **UNKNOWN stays visible** and is never folded into
pending or down. Every engine-tested row is validated with
`probe.DecodeRegionalHeartbeat`, and the checked-in fixture
[`regional_history.json`](../../internal/adapters/http/handlers/testdata/m5/regional_history.json)
must satisfy the same decoder in both directions.

The chart is the existing chart envelope (`buckets`, `downtime_intervals`,
`unknown_intervals`): latency buckets come only from this probe's measured
samples (never synthetic), arrays are never null, and both interval kinds are
preserved. Fixture:
[`regional_chart.json`](../../internal/adapters/http/handlers/testdata/m5/regional_chart.json).

Query semantics mirror the unqualified list exactly: `hours` (1–720, default
24), `limit` (default 100, cap 500) applied to the **most recent** rows before
the requested `order`, `important=true` filtering after a full-window scan.

## Scope note — section 7.2 activation (part 2)

The unqualified `GET /api/monitors/:id/heartbeats` and `/chart` endpoints are
now **activated** per section 7.2, together with the UI that consumes them:

- Rows carry `scope` and `latency_available`. A **local-only** monitor keeps
today's measured rows and buckets exactly (`scope: "local"`,
`latency_available: true` — only additive markers).
- A monitor with any remote member serves the **policy-evaluated overall
timeline**: synthesized segments (`scope: "overall"`, `latency_available:
false`, `ping: 0` — the unmeasured zero sentinel) derived from
`MonitorHealthService.History` intervals, never pooled regional samples. `id`
is a 1-based window sequence (interval identity is not persisted), `message`
is the bounded interval reason, and `important` marks status changes.
UNKNOWN stays visible as `"unknown"`.
- The overall chart carries **no synthetic latency buckets** — only downtime
runs (down/pending) and unknown runs of the overall timeline.
- The monitor detail UI selects regions for latency: the region picker switches
the response-time chart to the relationship-checked regional chart endpoint,
and the overall view shows the interval bands plus the latency-unavailable
hint instead of a fabricated curve.

The browser suite (`web/tests/e2e/09-regional-history.spec.ts`, with probes
elementabled in the e2e server) asserts the overall compatibility state and
the regional latency selection end to end; the full 13-spec suite verifies the
dashboard, monitor, status-page, RBAC, escalation and ack contracts against
the changed read shapes.

**Production wiring defect found by the browser test:**
`internal/bootstrap/run.go` constructed `MonitorHealthService` with a nil
access service, so every M5 regional route answered `monitor_not_found` even
for monitors the caller owned (the engine tests built the service with access
wired and could not see it). Fixed with `MonitorHealthService.SetAccess` in the
composition root; health reads without the choke point keep failing closed.

Also fixed here: the pre-existing e2e expectation in `01-login-dashboard` that
predated `unknown` joining `DEFAULT_STATUS_ORDER` (commit `7cc0c23`), and one
pre-existing prettier drift in `web/src/lib/stores/ws.svelte.ts` so the lint
gate is green.

## Files

| Responsibility | Owner |
|---|---|
| Evidence and relationship ports | `internal/core/ports/regional_history.go` |
| Relationship-checked read composition | `internal/core/services/monitor_regional_service.go` |
| Wire rows, chart and error codes | `internal/adapters/http/handlers/monitor_regional_history.go` |
| Section-7.2 overall stream and markers | `internal/adapters/http/handlers/heartbeat.go` |
| Route activation | `internal/adapters/http/router.go`, `internal/bootstrap/run.go` |
| Frozen fixtures | `internal/adapters/http/handlers/testdata/m5/regional_history.json`, `regional_chart.json` |
| TypeScript contracts and client | `web/src/lib/api/regional.ts` |
| Intervals-only chart mode and picker | `web/src/lib/components/ResponseTimeChart.svelte` |
| Latency selection | `web/src/routes/(admin)/monitors/[id]/+page.svelte` |
| Browser coverage | `web/tests/e2e/09-regional-history.spec.ts` |

No new dependency and no schema migration were added. The route guard in
`internal/adapters/probe/commands_test.go` no longer fences these routes
(their acceptance now exists); revoke/delete stay fenced. The frozen baseline
heartbeat contract (`testdata/v1/baseline/heartbeat.json`) gained the two
additive markers in the same commit.

## Verification

Use [the committed gate](../TESTING.md#m5-read-api-foundation) and
`scripts/m5_read_evidence.py`. Required named cases additionally include
`TestMonitorRegionalHistoryRelationship`,
`TestMonitorRegionalHistoryBoundsAreUTC`,
`TestMonitorRegionalHistoryFailures`, `TestMonitorRegionalHistoryFixtures`,
`TestMonitorRegionalHistoryQuerySemantics`, `TestMonitorRegionalHistoryChart`,
`TestMonitorRegionalHistoryHTTPErrors`, `TestM5HistoryAPI/sqlite` and
`TestM5HistoryAPI/mariadb`.

### Executed evidence — 2026-09-27

Commands executed (Go 1.26.6; `TEST_MARIADB_DSN` pointed at a disposable
`mariadb:11` database — the live engine matrix, not a skipped one; frontend
gates from `web/`):

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./... > /tmp/m5-full-race7.jsonl
python3 scripts/m5_read_evidence.py /tmp/m5-full-race7.jsonl
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
gofmt -l internal/
git diff --check
bun run check && bun run build && bun test src/lib
./node_modules/.bin/prettier --check . && ./node_modules/.bin/eslint .
GOTOOLCHAIN=go1.26.6 bunx playwright test --config=tests/e2e.config.ts
```

The fail-closed audit passed over the full-suite log and requires
`TestM5HistoryAPI/mariadb` and `TestM5HistoryAPI/sqlite` as passes.

Passed / failed / skipped: all 22 test packages passed with 4,211 named
test/subtest passes and zero failures (two longstanding unrelated skips). Zero
M5 skips. Frontend: `svelte-check` 0 errors/0 warnings, 255 unit tests pass,
prettier and eslint clean, CGO-free build and golangci-lint 0 issues.
Browser: **13/13 Playwright specs pass**, including the new
`09-regional-history` (overall latency-unavailable state, interval bands,
region picker, regional latency selection and back) and the login/dashboard,
http-monitor, notifications, status-page, RBAC, escalation, alert-ack and
navigation specs that verify the changed read shapes against every in-repo
consumer contract.

Contract parity is executable: every engine-tested row decodes with the frozen
`probe.DecodeRegionalHeartbeat`, the checked-in fixture decodes both
directions and equals the handler output field for field, the frozen baseline
heartbeat contract gained the two additive markers and requires them, and the
TypeScript contracts mirror the fixture shapes.

Mutation checks (both reverted, file hashes verified): bypassing the
relationship proof failed `TestMonitorRegionalHistoryRelationship` cases; making
the local-only detection ignore remote members failed
`TestMonitorRegionalOverallHistory/RemoteMemberServesOverallSegments` and
`TestM5HistoryAPI`.

The browser test also exposed a production wiring defect —
`MonitorHealthService` was built with a nil access service in
`internal/bootstrap/run.go`, so every M5 regional route answered
`monitor_not_found` for owned monitors in the real app while the engine tests
(green, with access wired) could not see it. Fixed with `SetAccess` in the
composition root. Also fixed: a pre-existing stale e2e expectation
(`01-login-dashboard`, predating `unknown` joining `DEFAULT_STATUS_ORDER`) and
a pre-existing prettier drift in `web/src/lib/stores/ws.svelte.ts`.

Acceptance criteria still unverified: fleet/monitor regional views (probe
fleet page, region status/diagnostics panels), full M5 browser acceptance
(grant revocation after subscription, reconnect, stale caches, pending config
and pending ACK — slice 5), production restart and load/performance.
Hashed evidence: [M5_REGIONAL_HISTORY_EVIDENCE.json](M5_REGIONAL_HISTORY_EVIDENCE.json).

## Next

The UI half of the slice: TypeScript contracts from these fixtures, fleet and
monitor views, English/Thai messages and browser tests, with UNKNOWN kept
visible and unavailable diagnostics labeled unknown. Then the section-7.2
compatibility activation (`scope` / `latency_available` on the unqualified
endpoints with the UI's latency moved to selected regional endpoints), and
browser events for full M5 acceptance.
