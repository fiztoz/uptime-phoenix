# M5 revisioned assignment writes

Status: third M5 slice, built on the [read API foundation](M5_FOUNDATION.md)
and [fleet diagnostics](M5_FLEET_DIAGNOSTICS.md). It implements the second of
the foundation's ordered next slices: **revisioned assignment writes**.
Durable operations, regional history and browser work remain untouched.

## What is implemented

| Surface | Behavior |
|---|---|
| `PUT /api/monitors/:id/probes` | Admin atomic complete desired-set replacement with a mandatory expected revision; 409 on stale |
| `POST /api/monitors` (extended) | Optional `probe_ids`, `health_policy`, `probe_bindings` create the monitor and its complete desired set in **one transaction**; omission keeps `["local"]`/`any_down` |
| `POST /api/monitors/:id/clone` | A clone of a remotely assigned monitor is **rejected** for a non-admin (403), never silently rerouted; an admin clone reproduces the source's complete set atomically |

All three run behind the existing auth middleware; the replacement route also
carries `middleware.RequireAdmin`. A non-admin creator retains today's local
behavior and receives 403 only when explicitly attempting remote membership —
and the forbidden create leaves **no monitor row** behind. Monitor updates with
no assignment fields never touch the set (asserted on both engines).

## Write contract (protocol section 7.1)

Request: `expected_revision` (mandatory decimal string), `probe_ids` (complete
member list), `health_policy` (`any_down`/`all_down`), `alert_delivery`
(optional, `regional` is the only implemented mode), `bindings` (optional array
of `probe_id`, `kind`, `binding_key`).

Response: the **same frozen desired-set view as GET**
[`assignments.json`](../../internal/adapters/http/handlers/testdata/m5/assignments.json)
shape — `revision`, `health_policy`, `alert_delivery`, and `assignments` with
`probe_id`, `generation`, `desired_config_revision`, `applied_config_revision`,
`sync_status` and resolved `bindings`. The write fixture
[`assignment_replace.json`](../../internal/adapters/http/handlers/testdata/m5/assignment_replace.json)
freezes the pending presentation.

**Pending application.** Members whose desired state this write changed (added,
re-bound, or the policy changed for everyone) read `sync_status: "pending"` in
the write response — even when a receipt proves an *earlier* document — because
no publication can cover the new desired state yet. A no-op write (revision
preserved) marks nobody and falls back to the digest-proven derivation. This is
how "saving desired state succeeds independently of remote connectivity, and
the UI shows pending application" is realized.

**Bindings.** Omitted `bindings` preserves retained members' bindings and
supplies none for new ones; an explicit list (including `[]`) replaces all
bindings and `[]` clears them. A new or recreated Docker assignment always
requires its explicit binding, and clearing is only valid when the resulting
set still satisfies every member's resource requirement.

## Validation before any commit

Every rule below fires before a single row changes, with the protocol section
7.3 status:

| Rejection | Status / code |
|---|---|
| Missing/non-decimal/non-positive/overflowing `expected_revision` | 400 `invalid_expected_revision` |
| Empty, duplicate or malformed `probe_ids` | 400 `invalid_probe_ids` |
| Unsupported `health_policy` / `alert_delivery` | 400 `invalid_health_policy` / `invalid_alert_delivery` |
| Malformed/duplicate/local/non-member bindings | 400 `invalid_bindings` |
| Stale expected revision (also a writer-side race) | 409 `stale_revision` |
| Unknown probe identity | 409 `unknown_probe` |
| Disabled registration (revoked class) | 409 `probe_unavailable` |
| Push monitor with remote members, remote member for a type with no installed pull checker, Docker remote member without its binding, binding on a non-Docker member, or a proxied monitor whose checker ignores proxies | 422 `unsupported_assignment` |
| Missing/hidden monitor | 404 `monitor_not_found` |

Remote probe capability advertisements (hello) are runtime evidence and stay a
publication/activation rejection; this write validates only what the hub build
knows (`checker.CapabilityInspector` behind the small
`ports.ProbeAssignmentCapabilities` port).

**Revision zero is never a valid precondition.** A legacy monitor without an
assignment set reads revision `0`; the mutation initializes the set *inside its
own transaction* and expects `expected_revision: "1"`. Nothing is created by a
read.

## Files

| Responsibility | Owner |
|---|---|
| Write port + capability port | `internal/core/ports/probe_assignment_write.go` |
| Compiled capability facts | `internal/adapters/checker/capability_inspector.go` |
| Atomic replace + create-with-set | `internal/adapters/repository/probe_registry.go` (`ProbeAssignmentStore`) |
| Complete-set validation, pending derivation, create/clone policy | `internal/core/services/probe_assignment_service.go` |
| HTTP request/response and error mapping | `internal/adapters/http/handlers/monitor_regional.go` |
| Create/clone handler paths | `internal/adapters/http/handlers/monitor.go` |
| Route activation | `internal/adapters/http/router.go`, `internal/bootstrap/run.go` |

The live write path reuses `ReplaceWithBindings` (live `Replace` semantics:
every member must be a currently enabled registration). Backup `Restore`
intentionally allows disabled identities and is deliberately not used here.

## Mutation-checked guards

Three load-bearing rules were verified by deliberate mutation (all reverted,
file hashes confirmed identical):

1. dropping the stale-revision precondition failed
   `TestProbeAssignmentServiceReplace/StaleRevisionWritesNothing`;
2. dropping the pending override failed `TestMonitorRegionalHTTPReplace`
   (fixture drift);
3. treating omitted bindings as an explicit clear failed
   `TestM5AssignmentWrites/sqlite` on the preserve case.

## Verification and handoff gate

Use [the committed gate](../TESTING.md#m5-read-api-foundation) and
`scripts/m5_read_evidence.py`. Required named cases now additionally include
`TestValidateDesiredAssignments`, `TestProbeAssignmentServiceReplace`,
`TestMonitorServiceCloneAuthority`, `TestMonitorRegionalHTTPReplace`,
`TestMonitorRegionalHTTPReplaceErrors`, `TestM5AssignmentWrites/sqlite` and
`TestM5AssignmentWrites/mariadb`. The engine tests drive the production Echo
router with signed JWTs over persisted state on both engines.

### Executed evidence — 2026-09-27

Commands executed (Go 1.26.6; `TEST_MARIADB_DSN` pointed at a disposable
`mariadb:11` database — the live engine matrix, not a skipped one):

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./... > /tmp/m5-full-race2.jsonl
python3 scripts/m5_read_evidence.py /tmp/m5-full-race2.jsonl
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
gofmt -l internal/
git diff --check
```

The fail-closed audit passed over the full-suite log and requires
`TestM5AssignmentWrites/mariadb` and `TestM5AssignmentWrites/sqlite` as
**passes**, so a silently skipped engine leg cannot satisfy it.

Passed / failed / skipped: all 22 test packages passed with 4,127 named
test/subtest passes and zero failures. Ten tests skipped: the two longstanding
unrelated skips (`TestDatabaseChecker_Check_MongoDB_RealServer`,
`TestTelegramSender_Send_DownSeverity`) plus eight live-network DNS/TCP checker
self-skips (public resolver unavailable in this run; they passed in the
preceding slice's run). Zero M5 skips. The 73 M5-relevant named passes include
both engine legs of `TestM5AssignmentWrites`, `TestM5FleetAPI` and
`TestM5ReadAPI`, the complete-set validation table, the revision/pending/legacy
service tests, the write fixture and the full error-mapping table. CGO-free
build passed, golangci-lint reported 0 issues, gofmt and `git diff --check`
were clean. Engine coverage: SQLite and live MariaDB both exercised through the
production Echo router with signed JWTs.

Mutation checks (all reverted, file hashes verified identical afterwards):
dropping the stale-revision precondition failed
`TestProbeAssignmentServiceReplace/StaleRevisionWritesNothing`; dropping the
pending override failed `TestMonitorRegionalHTTPReplace`; treating omitted
bindings as an explicit clear failed `TestM5AssignmentWrites/sqlite`.

Acceptance criteria still unverified: frontend/browser E2E, production-process
restart, load/performance, concurrent multi-writer races beyond the
in-transaction revision fencing, and the publication/activation-side capability
rejections (unchanged M4 behavior). `alert_delivery` accepts only `regional`.
No schema migration and no new dependency were added.
Hashed evidence: [M5_ASSIGNMENT_WRITES_EVIDENCE.json](M5_ASSIGNMENT_WRITES_EVIDENCE.json).

## Next

Slice 3 of the foundation order: durable administrative operations
(enrollment/rotation/revoke/reset) wrapped with exact operation receipts. It
must freeze where endpoint/pin metadata lives before implementing registration
POST, and must never return 202 without persisting a real operation. The route
guard (`TestNoUnimplementedProbeAdminRoutes`) still fences those paths.
