# M5 durable administrative operations

Status: fourth M5 slice, built on the [read API foundation](M5_FOUNDATION.md),
[fleet diagnostics](M5_FLEET_DIAGNOSTICS.md) and [assignment writes](M5_ASSIGNMENT_WRITES.md).
It implements the third of the foundation's ordered next slices: **durable
administrative operations**. Revocation, deletion and browser work remain.

## What is implemented

| Surface | Behavior |
|---|---|
| `POST /api/probes` | Registration create with the frozen network trust; 201 admin detail view, initially `unconfigured` |
| `PATCH /api/probes/:probe_id` | `name`, `location`, `enabled` with mandatory `revision`; 409 on stale |
| `POST /api/probes/:probe_id/enroll` | Write-only enrollment exchange wrapping the real connector; 202 exact receipt |
| `POST /api/probes/:probe_id/rotate-credential` | Wraps `ProbeCredentialRotationService.Issue` under the operation identity; 202 receipt |
| `POST /api/probes/:probe_id/reset-stream` | Wraps `PrepareStreamReset` under the caller-supplied identity; 202 receipt |
| `GET /api/probe-operations/:operation_id` | Admin operation state, phase and redacted errors |

All routes run `SessionOrAPIKey(authSvc, apiKeyRepo, "write")` then
`RequireAdmin`, return `no-store`, and honor `PROBES_ENABLED=false` with the
typed 503 after authentication. **`POST /api/probes/:probe_id/revoke` and
`DELETE /api/probes/:probe_id` stay unregistered** — there is no durable
revocation/soft-delete state yet, and an endpoint that answers 2xx without
performing the work is worse than no endpoint. The route guard still fences
them.

## The endpoint/pin freeze (decided before registration POST)

**Endpoint and TLS pin live on the registration** (`probes.endpoint`,
`probes.tls_fingerprint`), set at creation and **immutable afterward** —
changing them is the explicit identity workflow, never an ordinary update
(`ProbeRegistryStore.Update` cannot touch those columns by construction).
Enrollment copies them into the prepared connection; the client-visible
`ProbeView` reports the registration's values and falls back to a prepared
connection's for pre-freeze rows. Neither value is authentication material;
the protected credential, key hashes and stream/enrollment identities are
still never disclosed.

The registration create shape stores the operator's `host[:port]` suggestion
(the frozen `http-probe-create.json` shape); enrollment normalizes it into the
pinned runtime URL `wss://host[:port]/ws/probe/v1` (`services.RuntimeEndpoint`,
tested), and a canonical runtime URL passes through unchanged.

## Durable operations (never a fake 202)

`domain.ProbeOperation` + `ports.ProbeOperationRepository` +
`repository.ProbeOperationStore` (migration `071_probe_operations`, both
engines) persist the exact receipt: `operation_id`, `probe_id`, `status`
(`pending`/`running`/`succeeded`/`failed`), `phase` (bounded machine token),
`created_at`, `updated_at`, and `error` exactly when failed. The service
**persists the row before performing or acknowledging any work**; terminal
receipts are immutable history. Error codes are bounded tokens with fixed
redacted messages — no error chain, token or credential ever reaches a
receipt.

Wraps (all synchronous to their hub-side terminal state):

- **enroll** — prepares the connection from the frozen trust on first use, then
  consumes the write-only operator token over the pinned endpoint. The token is
  validated (`phx_probe_enroll_` prefix, ≥32 random bytes of suffix), consumed,
  never stored, logged or echoed. Phase `exchanging` → `exchanged`.
- **rotate-credential** — the operation identity IS the rotation ID (`Issue`
  receives it), so retries and status reads address the same durable rotation.
  Phase `exchanging` → `issued`.
- **reset-stream** — the caller supplies `enrollment_operation_id`, retained
  for retries; the immutable plan is prepared under it and the previous stream
  comes from the stored connection. Phase `preparing` → `prepared`; the new
  stream is proved only by later authenticated health.

A succeeded receipt proves the hub-side durable work only — remote
confirmation is separate evidence and is never implied. The existing service
contracts do the rejecting: an in-flight credential rotation and a prepared
stream reset exclude each other, and both wraps record their rejection as a
durable failed receipt (asserted on both engines).

## Wire contract

Requests are decoded with the **frozen M0 client decoders**
(`probe.DecodeProbeCreateRequest`, `DecodeProbePatchRequest`,
`DecodeRotateCredentialRequest`, `DecodeResetStreamRequest`), and every
response is validated against `probe.DecodeProbeView` /
`probe.DecodeOperationReceipt` in the tests — the registered routes cannot
drift from the wire contract. The engine test feeds the M0 fixtures themselves
(`http-probe-create.json`, `http-probe-patch.json`,
`http-rotate-credential-request.json`, `http-reset-stream-request.json`) as
request bodies.

### ProbeView correction (wire alignment)

Aligning with the frozen M0 decoder, `ProbeView` now renders: config revisions
as decimal strings with **`"0"` when unreported** (they were null), `capabilities` as an **empty array** when nothing is advertised (was null),
the three lifecycle statuses always in vocabulary (local reads
`active`/`online`/`ready`), and `protocol_version` as a nullable number. The
monitor assignment/region views keep their M5-foundation-frozen nulls — those
fixtures are their own contract and were not changed.

## Files

| Responsibility | Owner |
|---|---|
| Operation receipt model | `internal/core/domain/probe_operation.go` |
| Durable operation port | `internal/core/ports/probe_operation.go` |
| Dual-engine operation store | `internal/adapters/repository/probe_operation.go` |
| Registration writes + operation wraps | `internal/core/services/probe_admin_service.go` |
| Wire DTOs and routes | `internal/adapters/http/handlers/probe_admin.go` |
| Frozen network trust columns | migration `071_probe_operations`, `internal/core/domain/probe_registry.go` |
| Route activation | `internal/adapters/http/router.go`, `internal/bootstrap/run.go` |

No new dependency was added. The migration also teaches the test harness the
071 tail-heal (`mariadbTailHeals`, `healProbeNetworkIdentity`) so registry
migration rehearsals can rebuild `probes` at older boundaries without editing
historical migrations.

## Verification and handoff gate

Use [the committed gate](../TESTING.md#m5-read-api-foundation) and
`scripts/m5_read_evidence.py`. Required named cases now additionally include
`TestProbeAdminRegistrationLifecycle`, `TestProbeAdminOperations`,
`TestValidEnrollmentToken`, `TestRuntimeEndpoint`,
`TestProbeAdminContractParity`, `TestProbeAdminHTTPErrors`,
`TestM5AdminOperations/sqlite` and `TestM5AdminOperations/mariadb`.

### Executed evidence — 2026-09-27

Commands executed (Go 1.26.6; `TEST_MARIADB_DSN` pointed at a disposable
`mariadb:11` database — the live engine matrix, not a skipped one):

```sh
GOTOOLCHAIN=go1.26.6 go test -race -count=1 -timeout 2400s -json ./... > /tmp/m5-full-race3.jsonl
python3 scripts/m5_read_evidence.py /tmp/m5-full-race3.jsonl
GOTOOLCHAIN=go1.26.6 CGO_ENABLED=0 go build ./...
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run
gofmt -l internal/
git diff --check
```

The fail-closed audit passed over the full-suite log and requires
`TestM5AdminOperations/mariadb` and `TestM5AdminOperations/sqlite` as passes.

Passed / failed / skipped: all 22 test packages passed with 4,169 named
test/subtest passes and zero failures. Ten skips: the two longstanding
unrelated skips plus eight live-network DNS/TCP checker self-skips. Zero M5
skips. Contract parity is executable: requests are decoded by the frozen M0
decoders, every response passes `probe.DecodeProbeView` /
`probe.DecodeOperationReceipt`, and the engine test feeds the M0 fixture files
as request bodies on both engines. The registry migration rehearsal
(`LegacyBackfillAndDown`) passes on both engines with the 071 tail-heal.
CGO-free build passed, golangci-lint reported 0 issues, gofmt and
`git diff --check` were clean.

Mutation checks (both reverted, file hash verified identical afterwards):
removing the pre-acknowledgement persistence failed
`TestProbeAdminOperations/EnrollPersistsBeforeAcknowledgement`; removing the
runtime endpoint normalization failed `TestRuntimeEndpoint` and the engine
wrap assertions.

Acceptance criteria still unverified: frontend/browser E2E, production-process
restart (including migration 071 on a production database), load/performance,
remote application confirmation of rotations/resets (receipts prove hub-side
durable work only), revoke/soft-delete (routes stay unregistered), and the
endpoint/pin identity-change workflow (immutability is enforced; the workflow
itself is future work).
Hashed evidence: [M5_ADMIN_OPERATIONS_EVIDENCE.json](M5_ADMIN_OPERATIONS_EVIDENCE.json).

## Next

Revoke (`POST /api/probes/:probe_id/revoke`) and soft-delete
(`DELETE /api/probes/:probe_id`) need durable revocation/tombstone state and
the identity workflow for endpoint/pin changes first; they stay proposed.
Then regional history/chart routes and the browser slice.
