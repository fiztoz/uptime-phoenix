# M5 fleet diagnostics and admin read API

> Historical slice record. Remaining M5 work was completed on 2026-09-29; see
> [M5 completion and current acceptance](M5_COMPLETION.md).

Status: second M5 slice, built on the [read API foundation](M5_FOUNDATION.md).
It implements the first of the foundation's ordered next slices: **safe runtime
diagnostics and fleet read API**. Assignment writes, operations, regional
charts and browser work remain untouched.

## What is implemented

| Surface | Behavior |
|---|---|
| `GET /api/probes?limit=&cursor=` | Admin fleet list, exclusive-cursor pagination over probe ID order, `items` + `next_cursor` |
| `GET /api/probes/:probe_id` | Admin detail: `ProbeView` plus the protocol-authorized admin fields and explicit safe diagnostics |
| `GET /api/monitors/:id/probes` / `:id/health` | Formerly-null `sync_status`, `desired_config_revision`, `applied_config_revision`, `connection_status` and `config_sync_status` now carry evidence-based values |

Both fleet routes run `SessionOrAPIKey(authSvc, apiKeyRepo, "write")` then
`RequireAdmin`: an admin session or a write-scope API key owned by an admin is
required; a non-admin receives 403 and a read-scope key 401. `PROBES_ENABLED=false`
returns 503 `probes_disabled` after authentication, and a handler constructed
without the service returns 503 `fleet_unavailable`, exactly like the regional
reads. Successful reads are `no-store`. No route in this slice mutates anything.

## The safe read model

`domain.ProbeDiagnostics` composes exactly six durable evidence sources through
the dedicated `ports.ProbeDiagnosticsRepository`. The shared Bun store
(`repository.ProbeDiagnosticsStore`) reads all six inside **one transaction**,
so MariaDB and SQLite both return one coherent picture per probe from one
dialect-neutral implementation. The protected credential column and the
protected config payload column are never selected; a diagnostic read cannot
disclose what it does not load.

| Evidence source | Table | Disclosed facts |
|---|---|---|
| Registration | `probes` | id, key, name, location, kind, enabled, revision, timestamps |
| Enrollment | `probe_connections` | state, credential/certificate versions, certificate expiry, prepared/activated times; endpoint + TLS pin to admin detail only |
| Connection watchdog | `probe_watchdog_state` | status code, armed, pending loss, version, config revision, `incident_open` (identity collapsed), updated_at |
| Runtime lease | `probe_runtime_owners` + `probe_sessions` | owner, epoch/generation, deadline, transport `connected` |
| Config publication | `probe_config_snapshots` | latest revision, effective/stored times (digest kept server-side) |
| Applied receipts | `probe_active_configs` | revision, applied_at, assignment count (digest kept server-side) |

Never disclosed anywhere: `protected_credential`, key hashes, `stream_id`,
`enrollment_id`, `protected_payload`, plaintext configuration, document
digests. The tests scan every response for these.

## Derivation rules (evidence-bound)

`services.ProbeDiagnosticsSummary(at, facts)` is a pure function; `at` is the
current UTC instant and lease liveness is observational display state, never
lease authority for a write.

- **`sync_status`** — unreported without publication; `applied` **only** when
  the receipt's revision **and** SHA256 match the publication. Equal revision
  counters alone never prove application: the digest binds the exact source
  document, including its embedded assignment generations. A digest conflict at
  the newest revision is `rejected`; a receipt behind the publication (or
  absent) is `pending`.
- **`connection_status`** — `never_connected` without a session row, `online`
  only for a live connected session whose watchdog is not `suspect`/`lost`,
  `suspect` for a live connected session with a suspect/lost watchdog,
  otherwise `disconnected`. Transport health only.
- **`execution_status`** — `paused` for a disabled registration, `unconfigured`
  without enrollment, `ready` only while the runtime lease is held (deadline
  strictly after `at`), otherwise `degraded`. Execution readiness is derived
  separately from connection: a probe can be `online` and `degraded` at once.
  The local row is the hub scheduler and is `ready` while enabled.
- **`enrollment_state`** — `unconfigured`/`pending`/`active` from the stored
  connection state; null for `local` (no enrollment workflow).
- **`last_seen_at`** — the latest source-reported time (watchdog checkpoint or
  application receipt). Never a hub-side guess.
- Target availability is deliberately absent: it belongs to monitor health.

A lease exactly at its deadline is expired (`After`, not `>=`). Pause is not a
disconnect and not maintenance. Unreported values are always `null`, never an
implicit healthy default.

## Wire contract

`GET /api/probes` returns `{ "items": [ProbeView], "next_cursor": string|null }`
with `limit` (1–100, default 50) and `cursor` (exclusive probe ID).
`ProbeView` keeps the frozen protocol section 7 field list exactly:
`id`, `key`, `name`, `location`, `kind`, `enabled`, `enrollment_state`,
`connection_status`, `execution_status`, `last_seen_at`, `agent_version`,
`protocol_version`, `desired_config_revision`, `applied_config_revision`,
`queue_bytes`, `oldest_queued_at`, `revision`, `created_at`, `updated_at`.
Revisions are decimal strings.

Admin detail adds the protocol-authorized fields `endpoint`, `tls_fingerprint`,
`certificate_expires_at`, `credential_version`, `capabilities` and the
`diagnostics` object (`enrollment`, `connection`, `runtime`, `watchdog`,
`config` with `desired`, `applied`, `sync_status`) — documented as a protocol
section 7 extension in the same commit, as section 9 requires. The list never
carries endpoint or pin material.

Still null by design — there is no safe evidence source yet, and null means
unreported: `agent_version`, `protocol_version`, `queue_bytes`,
`oldest_queued_at`, `capabilities`. Later slices fill them only with evidence.

Executable contract:

- [fleet list response](../../internal/adapters/http/handlers/testdata/m5/fleet_list.json)
- [fleet detail response](../../internal/adapters/http/handlers/testdata/m5/fleet_detail.json)
- [Views and mappers](../../internal/adapters/http/handlers/probe_fleet.go)
- [derivation policy](../../internal/core/services/probe_fleet_service.go)

Errors keep `error` + `code`: `invalid_limit`, `invalid_cursor` (400),
`invalid_probe_id` (400), `probe_not_found` (404), `probes_disabled` and
`fleet_unavailable` (503). Storage errors are redacted. Legacy middleware 401/
403 responses keep their existing `{ "error": "..." }` shape.

## Files

| Responsibility | Owner |
|---|---|
| Nonsecret read model | `internal/core/domain/probe_diagnostics.go` |
| Safe read port | `internal/core/ports/probe_diagnostics.go` |
| Coherent dual-engine read | `internal/adapters/repository/probe_diagnostics.go` |
| Derivation + fleet composition | `internal/core/services/probe_fleet_service.go` |
| Wire DTOs and handlers | `internal/adapters/http/handlers/probe_fleet.go` |
| Monitor-level fills | `internal/core/services/monitor_regional_service.go`, `internal/adapters/http/handlers/monitor_regional.go` |
| Route activation | `internal/adapters/http/router.go` (`RouterOptions.ProbeFleet`), `internal/bootstrap/run.go` |

No schema migration and no new dependency were added; every column read
already existed. The nil-repository guard in `SessionOrAPIKey` was hardened so
a router constructed without an API-key repository cannot panic.

## Mutation-checked guards

Two load-bearing derivations were verified by deliberate mutation (both
reverted, file hashes confirmed identical):

1. dropping the SHA256 comparison (revision-only "applied") fails
   `TestProbeDiagnosticsSummaryBoundaries/AppliedNeedsRevisionAndDigest`;
2. ignoring lease expiry fails
   `TestProbeDiagnosticsSummaryBoundaries/ConnectionIsNotExecution` and the
   engine test's expired-owner assertions.

## Verification and handoff gate

Use [the committed gate](../TESTING.md#m5-read-api-foundation) and
`scripts/m5_read_evidence.py`. Required named cases now include
`TestProbeDiagnosticsSummaryBoundaries`, `TestProbeFleetServiceListAndDetail`,
`TestMonitorRegionalHTTPDiagnosticFills`, `TestProbeFleetHTTPFixtures`,
`TestProbeFleetHTTPErrors`, `TestProbeFleetHTTPSecretExclusion`,
`TestM5FleetAPI/sqlite` and `TestM5FleetAPI/mariadb` in addition to the
foundation's list. The audit rejects missing/failed/skipped required cases and
incomplete package output.

### Executed evidence — 2026-09-27

Commands executed (Go 1.26.6; `TEST_MARIADB_DSN` pointed at a disposable
`mariadb:11` database — the live engine matrix, not a skipped one):

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./... > /tmp/m5-full-race.jsonl
python3 scripts/m5_read_evidence.py /tmp/m5-full-race.jsonl
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
gofmt -l internal/
git diff --check
```

The fail-closed audit passed over the full-suite log and explicitly requires
`TestM5FleetAPI/mariadb` and `TestM5ReadAPI/mariadb` as **passes**, so a silently
skipped MariaDB leg cannot satisfy it.

Passed / failed / skipped: all 22 test packages passed with 4,068 named
test/subtest passes and zero failures. Two unrelated tests skipped
(`TestDatabaseChecker_Check_MongoDB_RealServer`,
`TestTelegramSender_Send_DownSeverity`); zero M5 skips. The 61 M5-relevant named
passes include both engine legs of `TestM5FleetAPI` and `TestM5ReadAPI`, the
fixture/error/secret-exclusion handler tests, the derivation boundary tests and
the monitor-level diagnostic fill test. CGO-free build passed, golangci-lint
reported 0 issues, gofmt and `git diff --check` were clean. Engine coverage:
SQLite and live MariaDB both exercised through the production Echo router with
signed JWTs and real API-key rows.

Mutation checks (both reverted, file hash verified identical afterwards):
dropping the SHA256 comparison in the sync proof failed
`AppliedNeedsRevisionAndDigest`; ignoring lease expiry failed
`ConnectionIsNotExecution`.

Acceptance criteria still unverified: frontend/browser E2E, production-process
restart, load/performance and fleet mutation workflows (no write endpoint
exists in this slice). `agent_version`, `protocol_version`, `queue_bytes`,
`oldest_queued_at` and `capabilities` remain null — there is no safe evidence
source for them yet. No schema migration and no new dependency were added.
Hashed evidence: [M5_FLEET_DIAGNOSTICS_EVIDENCE.json](M5_FLEET_DIAGNOSTICS_EVIDENCE.json).

## Next

Slice 2 of the foundation order: revisioned assignment writes with capability
validation, 409 on stale expected revision, and atomic monitor-create plus
explicit assignments. Nothing in this slice authorizes it: the diagnostics
reads are observational and every write must still prove lease authority and
revision preconditions inside its own transaction.
