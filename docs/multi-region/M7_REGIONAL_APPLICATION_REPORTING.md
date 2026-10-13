# M7 authenticated regional application reporting

Bounded continuation after local-hub source-local state (`dacad55`).
Implemented and verified on `feat/m7-receipt-backed-config` after `dacad55`
(2026-10-13). No aggregate activation is authorized.

## Shared implementation contract

Use a **separate** optional `delivery.regional_applied` frame, never infer
application from `config.applied`, desired fields or `paging.aggregate.v1`.
Capability `availability.regional_application.v1` promises only truthful reporting
of already persisted effective **regional** facts. It does not mean aggregate/
both is implemented, does not set cutover `ModeApplicationSupported`, and does
not authorize sends/suppression. Existing configuration/telemetry remain valid
without the capability. No HTTP View/UI or cutover runtime caller.

Two-sided negotiation:
- Hello advertises the exact compiled reporting capability.
- Optional welcome boolean `regional_application_reporting` is omitted when
  false; absence means false. Present values must be JSON booleans, not null.
  True requires hello's exact capability and a configured hub report repository.
- Older peers receive no new frame. The capability is never a required snapshot
  capability; existing checker/notifier/snapshot requirements stay unchanged.

Wire DTO `RegionalApplicationReport`: embeds `SessionIdentity`; `revision` is a
positive `Decimal`; `sha256` is the exact retained document digest;
`applications` is 1–256 `RegionalApplication` entries. Each entry has integer
`monitor_id`, positive decimal-string `assignment_revision` and `generation`,
and explicit `alert_delivery: "regional"`. Reject duplicates, malformed shapes,
foreign identities, nonregional modes and oversize batches. Unknown optional
fields retain V1 compatibility. No endpoints/settings/credentials/source clocks.

`DecodeRegionalApplicationReport` validates bounded shape, not authenticity.
The hub checks negotiation, trusted session target/stream/generation, and calls
`ports.RemoteRegionalApplicationRepository.RecordRemoteRegionalApplications(ctx,
 session domain.ProbeReplaySession, applications []domain.ProbeDeliveryModeApplication)`.
The method is implemented by `RemoteProbeConfigSyncStore`; no speculative reader.

Edge sender calls new core
`EdgeConfigService.ReadRegionalApplications(ctx, metadata domain.ProbeConfigMetadata,
 generation int64, streamID string) ([]domain.ProbeDeliveryModeApplication, error)`.
This reads durable selected-stream facts, authenticates their exact retained
snapshot using the shared `decodeActive` validator on that **one captured tuple**,
without a second active-record read, and checks active metadata and
accepted generation/stream. Missing/legacy facts stay absent; do not re-derive
facts from desired assignments. Send bounded batches only after config ACK is
written, on both new activation and same-document retry. Reconnect repeats facts;
there is no new report ACK/cursor or source timestamp. Queue ordering is not proof
of a committed hub receipt: the repository independently checks it.

Hub migration 078 (SQLite + MariaDB) stores immutable remote effective regional
facts keyed by probe/document/monitor/stream, with snapshot FK and regional-only
CHECK. No backfill. In a serializable transaction reuse `lockProbeSession` (stored
owner/generation/expiry, registered connection/stream, installation key, enabled/
revoked/reset guards); require exact active pointer/committed receipt/snapshot,
authenticate/decode retained bytes, compare every member's immutable document set
binding/generation, insert or compare exact existing facts, and recheck DB-clock
lease expiry after writes before commit. No enrichment from current desired rows.
A historical bound document may report its original binding after policy ABA;
these facts never become current assignment or cutover authority. A stale stream,
owner/generation, missing prerequisite, corrupt document or mismatched duplicate
refuses the whole batch. Preserve first recorded hub time/connection provenance.

Production wiring: `HubTransport.SetRegionalApplications(repo)` opts in only the
main remote connector; cmd/probe advertises the compiled capability. Admin-only
connectors without this repository remain unnegotiated. Local-hub facts stay in
077 and do not traverse remote-only wire frames.

## Verification required

Wire/handshake compatibility and precision, omitted false and strict optional
boolean, no capability substitution, no unnegotiated frames, old peers and legacy
zero bindings; genuine encrypted edge load and persisted fact selection, not
synthesis. Real SQLite/MariaDB receipt prerequisite, exact immutable tuple,
duplicate immutability, corruption/redaction/cancellation, stale lease/stream/
owner/reset and batch rollback. Compose real pinned TLS/WebSocket config session,
refused first receipt, cold reopen/reconnect, policy ABA, old/new document reports,
and desired regional/aggregate/both with real persisted effective regional facts.
Require exact new MariaDB leaves in the strict gate; keep aggregate decisions
unwired and regional sends active. No release/failure-matrix or deployment claim.

## Executed verification

Artifacts: ignored `tmp/m7-regional-reporting-verification/`. Tests/build use
cached Go 1.26.6, `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`
and a sanitized environment; fixture credentials are never logged.

- `focused-04.jsonl`: **113 named race passes, zero failures, three MariaDB skips**.
  Real SQLite config/session reporting across regional/aggregate/both desires,
  wire shape/precision/compatibility, and encrypted edge-read/authentication tests.
- `focused-05.jsonl`: **31 named race passes, zero failures, 18 MariaDB skips**.
  Real SQLite remote report contract; skips are not engine acceptance.
- `focused-08.jsonl`: **13 named race passes, zero failures/skips**. Real pinned
  TLS/WebSocket two-sided negotiation, unsolicited-frame refusal before storage,
  positive persisted regional facts, redacted repository refusal and post-ACK
  replay traffic proving unnegotiated sessions remain live.
- CGO-free `go build ./...`, `go vet ./internal/...`, `golangci-lint run` (**0 issues**),
  formatting/diff checks passed. All **14** strict-gate parser regressions passed.
  The gate now requires all **76 M7 markers**, including **28 new** exact remote
  report MariaDB markers; parent/SQLite passes cannot substitute.
- Five private `.mutations` overlays bypassed the committed receipt prerequisite,
  final expiry check, retry immutability, captured-fact validation and independent
  effective mode. Every mutant failed its intended assertions, not compilation;
  the unchanged control passed. Production files were not mutated.
- Bounded independent read-only production-path review found no correctness
  blocker. This is inspection, not executed database evidence.

- Corrected full strict gate: **22 packages, 5,464 named race passes, 451 MariaDB
  passes**, zero failures/MariaDB skips/missing required markers/malformed events.
  All **76 M7 markers**, including the **28 new report markers** and three real
  desired-mode TLS/session leaves, executed. Elapsed **1361.2 seconds**, exit 0.
  The five recognized helper/optional/platform skips remain separate: cycle helper,
  MongoDB real-server checker, edge crash child, Linux disk-full case and optional
  Telegram sender. This is backend slice acceptance, not the full failure matrix.

Commands executed (the strict runner creates a fresh disposable local server):

```sh
go test -race -count=1 -json -timeout 600s \
  ./internal/adapters/repository ./internal/adapters/probe \
  -run 'TestAssignmentRevisionSessionContract|TestRegionalApplication'
go test -race -count=1 -json -timeout 600s ./internal/adapters/repository \
  -run TestRemoteRegionalApplicationContract
go test -race -count=1 -json -timeout 600s ./internal/adapters/probe \
  -run TestRegionalTransport
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-regional-reporting-verification/dual-engine.jsonl
# Internally: go test -race -count=1 -json -timeout 2400s -p 4 ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
gofmt -l internal/ cmd/
git diff --check
go list ./...
```

The runner used the existing Colima Unix socket and official MariaDB 11 digest
`sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15`.
No inherited database/provider credentials or Go downloads were used. Gate
container/anonymous-volume removal succeeded; both pre-existing containers were
preserved, no new container remains, and the unused task-owned image was removed.
Colima configuration was untouched and the daemon remains running; AWS was not used.
Private startup stderr capture changed no gate commands or coverage policy. Final
production/tests/scripts were covered; subsequent changes were comments/docs.

Counts include parent/subtest events, not unique functions; separate runs are not
additive. No frontend/Helm changes or new dependencies.

### Failed-attempt ledger

Preserve the failures rather than inferring a passing lifecycle from an ACK:

1. The first session test stopped at the ACK and lacked prepared hub connection/
   accepted-stream rows. Trace showed a real report read/send/receive followed by
   storage `not found`. Prepare through `ProbeConnectorStore` with protected fixture
   credentials and await the durable report. The earlier speculative timing/retry
   changes were removed: the actual hub application callback is sequential and
   waits for receipt commit; storage still proves that prerequisite independently.
2. New transport fixtures originally used inconsistent enrollment IDs and then
   advertised readiness with config revision zero. Consistent enrollment identity
   and honest startup readiness fixed the fixtures without weakening production
   authentication or health validation. Private trace overlays established the
   fail path; no tracing remains in production files.
3. Two strict-gate attempts executed **zero tests** because the detached process
   inherited the context-tool's ephemeral `TMPDIR`, which vanished on tool return.
   Private startup stderr identified the missing work directory. The corrected
   runner uses a task-owned durable dot-prefixed temporary directory and preserves
   both failures; it does not relax the strict gate or inherit provider credentials.

## Remaining authority and release boundaries

Remote authenticated regional reports have no consumer in the cutover evaluator.
They do not negotiate aggregate/both support, establish current desired identity,
prove cessation of regional sends or authorize provider I/O. Local-hub source facts
retain their separate 077 lifecycle: receipt retries do not backfill, legacy direct
send remains unchanged/unreported, and pre-send reads remain observational. No
report ACK/cursor means lost reports are retried through normal config reconnect;
there is no separate guaranteed-delivery queue in this slice.

The follow-on [delivery preparation slice](M7_DELIVERY_PREPARATION.md) builds exact
remote requests on these committed facts, without converting the facts or request
receipts into effective aggregate application or cutover authority.

Aggregate application, committed barriers, durable aggregate incidents/intents,
queued/in-flight suppression and final DB/provider-I/O fencing remain future work.
Edge metadata accounting remains its existing bounded subset. `make gate-full`
was not rerun; the historical reachable Go standard-library advisories remain a
separate release blocker. Full failure/load/migration/soak/canary acceptance is not
claimed. Nothing is pushed, deployed or activated by this slice.
