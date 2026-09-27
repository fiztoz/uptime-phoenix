# M5 regional history and chart routes

Status: fifth M5 slice (part 1 of "Regional history and UI"), built on the
[read API foundation](M5_FOUNDATION.md), [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md),
[assignment writes](M5_ASSIGNMENT_WRITES.md) and
[administrative operations](M5_ADMIN_OPERATIONS.md). It implements the
**relationship-checked regional history/chart routes**. The UI half of the
slice (fleet/monitor views, latency selection, English/Thai messages, browser
tests) and the section-7.2 compatibility activation follow separately.

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

## Scope note (what this slice deliberately does not change)

The unqualified `GET /api/monitors/:id/heartbeats` and `/chart` endpoints are
**unchanged** here. The section-7.2 compatibility change (marking them
`scope: "overall"` / `latency_available: false` and switching the UI's latency
to selected regional endpoints) is an explicit compatibility change that must
be verified against every dashboard, badge, status-page and external client
contract before activation; it lands with the UI half of this slice.

## Files

| Responsibility | Owner |
|---|---|
| Evidence and relationship ports | `internal/core/ports/regional_history.go` |
| Relationship-checked read composition | `internal/core/services/monitor_regional_service.go` |
| Wire rows, chart and error codes | `internal/adapters/http/handlers/monitor_regional_history.go` |
| Route activation | `internal/adapters/http/router.go`, `internal/bootstrap/run.go` |
| Frozen fixtures | `internal/adapters/http/handlers/testdata/m5/regional_history.json`, `regional_chart.json` |

No new dependency and no schema migration were added. The route guard in
`internal/adapters/probe/commands_test.go` no longer fences these routes
(their acceptance now exists); revoke/delete stay fenced.

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
`mariadb:11` database — the live engine matrix, not a skipped one):

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./... > /tmp/m5-full-race4.jsonl
python3 scripts/m5_read_evidence.py /tmp/m5-full-race4.jsonl
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
gofmt -l internal/
git diff --check
```

The fail-closed audit passed over the full-suite log and requires
`TestM5HistoryAPI/mariadb` and `TestM5HistoryAPI/sqlite` as passes.

Passed / failed / skipped: all 22 test packages passed with 4,195 named
test/subtest passes and zero failures. Ten skips: the two longstanding
unrelated skips plus eight live-network DNS/TCP checker self-skips. Zero M5
skips. Contract parity is executable: every engine-tested row decodes with
the frozen `probe.DecodeRegionalHeartbeat`, and the checked-in fixture
satisfies the same decoder in both directions and equals the handler output
field for field. CGO-free build passed, golangci-lint reported 0 issues, gofmt
and `git diff --check` were clean.

Mutation check (reverted): making `probeRelated` return true unconditionally
failed `TestMonitorRegionalHistoryRelationship/ClosedHistoryOutsideWindowIsUnrelated`
and `/UnrelatedProbeReadsAsMissing`. The restorative `git checkout` also
reverted the uncommitted service work; the edits were re-applied and the
recorded full-suite run is the post-restore one.

Acceptance criteria still unverified: the UI half of the slice (TypeScript
contracts, latency selection, fleet/monitor views, English/Thai messages,
browser tests), the section-7.2 compatibility activation (the unqualified
endpoints are deliberately unchanged here), frontend/browser E2E, production
restart and load/performance.
Hashed evidence: [M5_REGIONAL_HISTORY_EVIDENCE.json](M5_REGIONAL_HISTORY_EVIDENCE.json).

## Next

The UI half of the slice: TypeScript contracts from these fixtures, fleet and
monitor views, English/Thai messages and browser tests, with UNKNOWN kept
visible and unavailable diagnostics labeled unknown. Then the section-7.2
compatibility activation (`scope` / `latency_available` on the unqualified
endpoints with the UI's latency moved to selected regional endpoints), and
browser events for full M5 acceptance.
