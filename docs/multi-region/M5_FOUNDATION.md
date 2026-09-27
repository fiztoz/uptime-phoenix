# M5 foundation and continuation contract

Status: first read API slice, built on M0–M4 review fixes in `7bc81c5` (original
base `5873a79`). The second slice — safe runtime diagnostics and the admin fleet
read API — is recorded in [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md). M5 is
**not complete**. Inspect the actual HEAD and working tree before continuing or
allocating files.

## Implemented boundary

| Endpoint | Implemented behavior |
|---|---|
| `GET /api/monitors/:id/probes` | Visible monitor's desired assignment revision/policy, safe labels, generations and logical binding references |
| `GET /api/monitors/:id/health?hours=24` | Current overall and regional health, persisted projection version and duration-based coverage over the requested window |

Both routes use the existing session JWT middleware, then `AccessService` through
`MonitorHealthService`. A scoped non-admin can read a granted monitor, including
through existing group grants; being able to create a monitor does not grant fleet
administration. Missing and hidden monitors both return the same 404. Permission
changes use existing AccessService invalidation. Successful reads are `no-store`.

`PROBES_ENABLED=false` returns 503 `probes_disabled` after authentication. The
bootstrap wires this flag in API/all modes independently of connector ownership;
an API replica does not run a connector merely to serve these reads. A router
constructed without the M5 handler also registers these paths as unavailable.
Existing heartbeat, public status, browser WebSocket and local scheduling paths
are unchanged.

No fleet CRUD, enrollment/rotation/revoke/reset operation API, assignment writes,
regional heartbeat/chart endpoint or browser event is implemented by this slice.
This slice alone does not complete protocol section 7 or M5's first checklist
item; that item is completed by the [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md)
slice.

## Frozen JSON fixtures

The executable contract lives in:

- [health response](../../internal/adapters/http/handlers/testdata/m5/health.json)
- [assignment response](../../internal/adapters/http/handlers/testdata/m5/assignments.json)
- [explicit Views and mappers](../../internal/adapters/http/handlers/monitor_regional.go)
- [fixture and error assertions](../../internal/adapters/http/handlers/monitor_regional_test.go)

Change these together with any future frontend types or HTTP consumer. Never
serialize `domain.*` directly. In particular:

- `revision`, `generation` and `projection_version` are decimal **strings**. Keep
  them as strings in JavaScript; the fixtures deliberately exceed `2^53 - 1`.
  `monitor_id` retains the existing numeric API convention.
- Regional statuses are `up`, `down`, `pending`, `maintenance`, `unknown`. They are
  not the uppercase probe protocol or the legacy WebSocket `paused` mapping.
- A fresh result may have a late `received_at`; freshness comes from `observed_at`.
  Expiry is inclusive: at `fresh_until`, evidence becomes UNKNOWN. Future source
  timestamps are UNKNOWN. A paused monitor has paused counts and UNKNOWN regional
  status with reason `paused`; pause is not maintenance or connection failure.
- No evidence or a stale generation has null observation/receipt/deadline fields.
  `reason` contains a bounded diagnostic code, never a raw check output or error.
- `connection_status`, `config_sync_status`, assignment `sync_status`, and desired/
  applied config revisions are null in the frozen fixtures. They are now filled
  with evidence from the safe diagnostics port ([fleet diagnostics](M5_FLEET_DIAGNOSTICS.md))
  and stay null only where evidence is absent. Null means unreported; it does not
  mean online, applied, pending or failed. Never infer application from equal
  revision counters or infer a connection from an UP target check.
- Assignment `bindings` and health `regions` are arrays, including when empty.
  Bindings contain `kind` and `binding_key` only. No socket paths or credentials.
- `hours` defaults to 24 and must occur once as an integer from 1 to 720. It changes
  coverage only; current status is always evaluated at the current UTC instant.
  Coverage uses the existing historical assignment intervals and duration-based
  evaluator, never pooled sample counts. An undefined percentage is null, not zero
  or 100. PENDING/paused time counts as unknown; maintenance is excluded.
- `projection_version` is a persisted work version, not an ETag. Current freshness
  can expire before that version advances. Do not cache current health solely by
  version. Current evidence, history and version are observational reads, not one
  database snapshot or an authorization token for writes.
- Legacy monitors without an assignment row read as local with revision `"0"`.
  Reads never create rows. A future mutation must initialize under its transaction;
  revision zero is not a valid replacement precondition.

Handler errors retain `error` plus `code`: `invalid_monitor_id`, `invalid_hours`,
`invalid_request` (400), `monitor_not_found` (404), `probes_disabled` and
`regional_unavailable` (503). Storage errors are redacted. Existing authentication
middleware still supplies its legacy 401 `{ "error": "..." }` response; clients
must tolerate an absent code there. No new permission dimension is introduced.

## Reuse these paths

| Responsibility | Owner |
|---|---|
| Principal and visibility | Existing JWT middleware and `AccessService` |
| Policy, expiry and historical coverage | `MonitorHealthService`, `EvaluateMonitorHealth`, `CalculateHealthCoverage` |
| Authorized read composition and safe diagnostic reasons | `MonitorRegionalService` and `RegionalDisplayHealth` |
| HTTP field names, strings/nulls and redacted errors | `MonitorRegionalHandlers` |
| Route activation | `RouterOptions.RegionalMonitors`, wired in `internal/bootstrap/run.go` |
| Desired assignment atomicity | Existing `MonitorProbeAssignmentRepository` |
| Runtime authority and applied receipts | Existing config/connector/command repositories; not these read results |

The new service reads registrations by the already-authorized assignment IDs.
It never lists the fleet or opens protected configuration or credentials. Extend
it with a dedicated safe read port when diagnostic data is available; do not give
handlers a DB or a credential decoder. The internal current-health result now
carries the assignment set used for that evaluation and accepted receipt times,
so labels and generations do not require guessing from unrelated observations.

## Next slices, in order

1. **Safe runtime diagnostics and fleet read API.** ✅ Done — see
   [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md): nonsecret read model over the
   six evidence sources read coherently on both engines, digest-proven `applied`,
   connection/execution separation, paginated admin list/detail with admin
   session and write-scope API keys, and evidence-based replacement of the null
   diagnostic fields. `agent_version`, `protocol_version`, `queue_bytes`,
   `oldest_queued_at` and `capabilities` remain null until their own evidence
   ports exist.
2. **Revisioned assignment writes.** ✅ Done — see
   [assignment writes](M5_ASSIGNMENT_WRITES.md): complete-set validation before
   the atomic write (capability/resource rules, duplicate/unknown/disabled
   members, push-remote), omitted versus explicit-empty bindings, 409 stale
   expected revision over live `Replace` semantics, atomic create with an
   explicit set, and the non-admin clone rejection. `alert_delivery` accepts
   only `regional`; probe-side capability advertisements remain enforced at
   publication and activation.
3. **Durable administrative operations.** ✅ Done — see
   [administrative operations](M5_ADMIN_OPERATIONS.md): the endpoint/pin home
   is frozen on the registration (create-only identity), registration POST and
   PATCH land, and the existing installation/rotation/reset services are
   wrapped with durable exact operation receipts (persisted before any 202,
   redacted bounded errors). Revoke and soft-delete stay proposed: no durable
   revocation state exists yet and a fake 2xx is forbidden.
4. **Regional history and UI.** Implement relationship-checked history/chart
   routes before building latency selection. Use these fixtures for TypeScript
   contracts, then add fleet/monitor views, English/Thai messages and browser
   tests. Keep UNKNOWN visible; label unavailable diagnostics as unknown.
5. **Browser events and full M5 acceptance.** Add explicit wire mappings and
   monitor/admin fan-out checks. Test grant revocation after subscription,
   reconnect, stale caches, pending config and pending ACK; require the actual
   remote application receipt before showing confirmation.

When delegating, write the shared request/response contract first. Suitable
ownership boundaries are the safe read adapter/port, the HTTP service/handler,
then distinct frontend pages. `router.go`, `bootstrap/run.go`, protocol docs and
shared fixtures each need one integrator. No files are currently delegated.

## Verification and handoff gate

Use [the committed gate](../TESTING.md#m5-read-api-foundation) and
`scripts/m5_read_evidence.py`. The audit rejects missing/failed/skipped required
cases and incomplete package output; a suite that silently skipped MariaDB is
not accepted evidence. Test names:

- `TestMonitorRegionalServiceScopeAndFreshness`
- `TestMonitorRegionalServiceLegacyAndFailures`
- `TestRegionalDisplayHealthBoundaries`
- `TestMonitorRegionalHTTPFixtures`
- `TestMonitorRegionalHTTPErrors`
- `TestMonitorRegionalHTTPUnknownAndEmpty`
- `TestM5ReadAPI/sqlite` and `TestM5ReadAPI/mariadb`

The engine tests run persisted replay → actual services → production Echo router
with signed JWTs. They check scoped/admin success, unauthenticated/invalid JWT
rejection, matching hidden/missing responses after grant revocation, unrelated
fleet exclusion, disabled routes and safe JSON. Unit tests add exact freshness
boundaries, missing generations, pause/maintenance, large revision strings,
undefined percentages, empty arrays and error redaction.

Executed gate evidence is recorded below after verification. No UI, deployment,
process restart, load benchmark or fleet mutation acceptance is claimed here.

### Executed evidence — 2026-09-27

Commands executed (Go 1.26.6; a dedicated disposable MariaDB DSN was exported):

```sh
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./...
rtk proxy python3 scripts/m5_read_evidence.py /private/tmp/m5-full-race.jsonl
rtk proxy env GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
rtk proxy env GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
rtk proxy env GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/govulncheck ./...
rtk proxy git diff --check
```

Engines and named tests exercised: all eight required entries above passed,
including `TestM5ReadAPI/sqlite` and `TestM5ReadAPI/mariadb`. The audit also rejects
synthetic missing-engine, skipped-engine, failed-test, incomplete-package and
malformed evidence inputs; an empty log cannot pass.

Passed / failed / skipped: full race suite passed all 22 test packages, with
4,036 passing test/subtest events and zero failures. Two unrelated tests skipped:
`TestDatabaseChecker_Check_MongoDB_RealServer` and
`TestTelegramSender_Send_DownSeverity`; another 13 packages had no tests. CGO-free
build passed and final golangci-lint reported zero issues. Govulncheck found
zero vulnerabilities affecting the code or imported packages; three required-module
vulnerabilities were in affected code that is not called.

Acceptance criteria still unverified: frontend/browser E2E, production-process
restart, load/performance and fleet mutation workflows were not exercised. No M5
UI or diagnostic read adapter exists yet. No new schema or dependency was added
for M5; migration 070 belongs to the preceding M0–M4 corrections and remains part
of the prerequisite M0–M4 fix commit.
