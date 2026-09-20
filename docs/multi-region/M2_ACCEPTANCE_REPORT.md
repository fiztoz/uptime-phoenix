# M2 integrator acceptance — 2026-09-20

**Result: M2 engineering acceptance passed.** M0 contract regressions and M1 local
parity corrections also passed. The validated changes after `a2551f8` were committed
as `18762e7` (M1 corrections) and `32c26a8` (M2 runtime) on
`codex/multi-region-probe-plan`. Nothing was pushed or deployed.
This is not completion of M3–M6 or fleet release approval.

## Observed behavior

| Requirement | Independent evidence |
|---|---|
| Separate edge startup/storage/identity | `cmd/probe` tests and real init/run/inspect; private files; duplicate process rejected; stable stream, certificate and key; missing retained key fails closed |
| HTTP/TCP/DNS and existing notifiers | Installed checker/provider validators and tests; edge rejects unsupported assignments; actual HTTP target and webhook process flow |
| Pinned TLS/enrollment | Real TLS tests reject wrong/missing pins, wrong credentials and replayed enrollment; exact trusted identity/version comparison; protected hub credential preparation and hash-only edge token storage |
| Bounded session supervision | Race tests for reader/writer ownership, caller cancellation, control/replay fairness, malformed payloads and stale generation; runtime tests preserve incumbent on invalid newcomer and join shutdown |
| DB connector ownership | SQLite and live MariaDB competing-worker, takeover, stale callback and generation tests; real two-worker process retains one stable session across renewal |
| Atomic config application | Exact original bytes protected and committed with active pointer/indexes; injected commit failure returns rejection without revision advancement |
| Durable direct delivery | Atomic check/state/incident/intents; reserved outcome capacity; late counter fault rollback; failed provider attempt survives actual offline process restart |
| Offline health/restart | Hub stopped completely, edge remains ready and checks; same incident receives DOWN then UP; pending delivery, config, stream and sequence survive restart |
| Probe surface isolation | Actual process returns 404 for `/`, `/api/auth/register`, `/api/monitors` and `/api/users`; only TLS probe/health routes are registered |

The final M2 process run used CGO-free binaries and a fresh disposable MariaDB with
two normal hub workers. The edge's source sequence advanced **17 → 36**, management
generation **2 → 3**, accepted config stayed at **1**, and its stream UUID remained
unchanged. Provider responses were **503, 200, 200**; successful notifications were
one DOWN and one UP on the same persisted incident. No hub was running during the
failed delivery, edge restart, retry and recovery.

The final M1 process regression observed one DOWN and UP per monitor, one initial
escalation, a valid absolute ACK link, cancellation of later escalation, and no
duplicate resend after restarting both workers. Counts: direct-first=2,
direct-second=2, escalation=1, later=0.

## Verification commands and logs

Commands use `rtk proxy`, `GOTOOLCHAIN=go1.26.6` and
`GOCACHE=/private/tmp/phoenix-go-cache-template-race`.

| Gate | Result and retained evidence |
|---|---|
| `make gate-full` | Exit 0: `/private/tmp/phoenix-m2-gate-full.log`; build, vet, full race, 0 lint issues, Svelte 0 errors/warnings, frontend unit/build/lint, 12 browser tests, Helm matrix/validation, vulnerability check and diff check |
| Final `go test -race -count=1 ./...` | Exit 0: `/private/tmp/phoenix-m2-final-race.log` |
| Final `golangci-lint run` | 0 issues: `/private/tmp/phoenix-m2-final-lint.log` |
| Live DB `go test -race -count=1 -p 1 ./internal/adapters/repository/...` | Exit 0: `/private/tmp/phoenix-m2-live-db-final.log`; explicit `TEST_MARIADB_DSN` points to disposable `phoenix_m2_final_ci`, not an unset/skipped MariaDB run |
| Final hub/probe/operator build | `CGO_ENABLED=0 go build` for `cmd/app`, `cmd/probe`, `cmd/phoenix-probe-admin` succeeded |
| Linux portability | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/probe` succeeded |
| Real M1 process smoke | `/private/tmp/phoenix-m1-final-process/report.json`; command `scripts/multi_region_smoke.py --applied-outbox` with fresh local `_smoke` DB |
| Real M2 process smoke | `/private/tmp/phoenix-m2-final-process/report.json`; command `scripts/probe_runtime_smoke.py` with final binaries and fresh local `_smoke` DB |
| Targeted TLS/session/connector checks | `/private/tmp/phoenix-m2-hub-transport-race.log`, `phoenix-m2-runtime-session-race2.log`, `phoenix-m2-connector-service-race.log`, `phoenix-m2-hub-connection-final-race.log` |

The full gate's vulnerability check found no called vulnerable symbols or imported
vulnerable packages; three required modules had findings outside called code. No
dependency was added by this change. Production core code has no framework/driver
imports. Existing core tests that import an in-memory adapter are unchanged.

The first process fixture used `+00:00`; strict wire validation correctly rejected
it. The fixture now uses UTC `Z`. The first full MariaDB run exposed a migration
test leaving 045 without additive 050–051, and a failed retry inherited that damaged
schema/ledger. Tests now restore current schema and clean up injected triggers.
The final complete suite passed on a fresh DB. These failed runs were not counted
as successful evidence.

## Delegation and teaching

Antigravity, on Gemini 3.8 Flash High, implemented bounded pinned-client, identity,
session and edge-delivery persistence slices. Codex specified disjoint ownership,
reviewed source, reproduced defects, integrated the actual runtime and independently
verified all gates above. See the slice review files and
[the retrospective](../postmortems/2026-09-20-m1-integration-followup.md).

The quota expired during the runtime assignment. Codex explicitly took over those
files and sent a replacement read-only audit contract after reset; no model switch
or overages were used. Antigravity read that contract, source and retrospective and
completed the additional [read-only audit](M2_RUNTIME_SESSION_REVIEW.md). Codex
checked its concerns against source and independently reran focused race tests;
see [the integrator disposition](M2_RUNTIME_SESSION_INTEGRATOR_REVIEW.md).
The earlier one-time log-read blocker is resolved. The audit did not replace
the independent integration acceptance above.

## Next milestone boundary

The subsequent [first M3 increment](M3_CONFIG_SYNC_ACCEPTANCE.md) now implements
automatic snapshot construction and fenced durable hub application receipts.
The paragraph below records the boundary at the original M2 acceptance.

Use [the operator guide](M2_OPERATOR_GUIDE.md) for the supported manual flow.
Remote automatic snapshot construction, replay/ACK, retention/gaps and watchdogs
remain M3. Remote ACK/rotation/revoke/reset commands and escalation remain later
work, and regional fleet UI is M5. M2 explicitly rejects unsupported capabilities.
Its separate fixed 64 MiB queues stop visibly at capacity without dropping evidence;
they are not a long-running retention implementation. Provider delivery remains
at-least-once where an external timeout leaves the actual send outcome ambiguous.
