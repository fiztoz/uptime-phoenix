# M7 delivery-mode preparation (bounded, no mode activation)

Continuation after `af43c36`. **Implemented and verified** as a bounded remote
preparation foundation. This is not aggregate activation or release acceptance. Regional
execution remains unchanged; there is no aggregate send, suppression, applied-mode
capability, cutover barrier, new HTTP control or UI selector.

## Shared contract and ownership

Reuse `probe_commands`, command protection/dispatch/result persistence and source
`edge_applied_commands`. Add only `delivery.prepare`, not `delivery.activate`.
The exact capability is `command.delivery_prepare.v1`; it promises durable request
preparation only, never `ModeApplicationSupported`. Both existing regional report
negotiation and this command capability must be present before automatic issuance.
Unconfigured/admin transports do not issue. Unsupported pending requests cannot
starve supported command kinds.

`domain.ProbeDeliveryPreparation` embeds `ProbeDeliveryModeApplication` (requested
mode aggregate/both), plus `CommandID`, `CreatedAt`, `ExpiresAt`, `PayloadHash`.
This is a REQUEST, never effective application evidence. Binding includes hub,
probe, monitor, assignment-set revision, generation, config revision/hash and
accepted stream. Counters use existing positive canonical decimal-string wire
encoding except integer monitor ID. Assignment/document revisions are independent.

`delivery.prepare` uses existing command.request: target.probe_id, positive
assignment_generation, null source_alert_id. Data has hub_id, stream_id, monitor_id,
assignment_revision, config_revision, sha256, alert_delivery; only aggregate/both.
Generation is target.assignment_generation (no second identity space). Closed DTO
strict recognized fields, compatible unknown optional fields. No credentials,
endpoints, settings, source clock authority or activation token.

Hub issuance requires the exact committed active/receipt/authenticated retained
snapshot AND stored regional report for each requested member AND current desired
set revision/generation/mode. Stale historical reports remain legitimate reports
but cannot issue current preparation. Transaction fences stored session owner,
generation, stream and DB-clock expiry. Requests use deterministic tuple-bound IDs;
reconnect/duplicates preserve original protected bytes, expiry, and provenance.
Use existing capacity reservation and fixed one-day request lifetime. No fabricated
operator identity: automatic requests have requested_by=0.

Source recovery validates current accepted authority before returning an existing
exact request receipt, even after expiry/config changes. New requests authenticate
one captured protected config and facts, compare the requested mode to the desired
field only as a preparation prerequisite, and atomically revalidate that captured
snapshot/index/facts with stored identity before persisting a pending tuple plus
immutable command receipt. Missing/stale binding returns a durable rejected receipt;
corrupt/authentication failures return a redacted error, not success or repair.
Pending tuples never change effective regional facts, recording, send authorization,
telemetry or config activation. Success code delivery_prepared means only that the
PREPARATION command was recorded. Duplicate receipt recovery never constitutes
current preparation eligibility or mode application.

Private edge migration016 stores exact prepared request tuples keyed by command ID,
with deferred command-receipt FK, positive counters, aggregate/both CHECK and exact
snapshot FK. It transactionally rebuilds the command ledger with all existing
columns/constraints preserved, permitting the preparation success code only for
its own kind; old success remains code-empty. Populated old ACK receipt round-trip
and old-kind code refusal are tested. Downgrade refuses live preparation tuples or
receipts rather than silently losing them. No backfill. Existing hub command schema is reused (no new hub table).

File ownership during parallel work:
- Main: new domain/ports contracts; core edge prepare service; edge preparation
  storage/migration/tests; cmd/probe and bootstrap wiring; gate/docs/integration.
- Wire worker: commands.go, NEW delivery_preparation_codec.go/tests,
  command_runtime.go, runtime_session.go, hub_transport.go; command transport tests.
- Hub worker: probe_command.go, NEW probe_delivery_preparation.go/tests,
  probe_config_sync.go concrete store setters if necessary, core/services/
  probe_command_service.go; no other files. Parent integrates command codec wiring.

## Acceptance before local commit

Focused unit and real encrypted edge lifecycle tests; strict closed wire shape,
large revision 9007199254740993, old/unsupported peers; real SQLite/MariaDB committed
receipt+regional-report prerequisites, current binding/ABA, stream/session expiry,
immutable duplicate requests/results, rollback/corruption/cancellation and bounded
capacity. Live pinned TLS config/report -> prepare -> durable result, cold restart/
reconnect and no effective mode changes. Add exact MariaDB leaf requirements to the
strict disposable gate; run build, race, vet, lint, formatting, boundaries and links.
No release or operational activation claim; historical vulnerability gate remains.

## Sequencing limits

Preparation is not quiescence, mode application, source stopping proof, or a send
permit. A future cutover must define readiness/barrier/application ordering without
waiting circularly on aggregate-only suppression. All local/remote assigned members
remain required; offline/paused members do not disappear. Both may be a bridge but
is not implemented here. Queued/in-flight provider work, crash ambiguity, final
DB/provider-I/O fencing and durable aggregate incidents/intents remain separate.

This slice prepares remote edges only; the local member is not silently excluded
from a future all-source barrier. The subsequent
[protected local request seam](M7_LOCAL_DELIVERY_PREPARATION.md) records local pending
requests on fresh activation and upgrades missing records through new publication;
it adds no application support, execution gate or shared barrier. Existing source reset archives/retains old pending
rows without rebinding them to the new stream. Deterministic request identity keeps
terminal expired/rejected receipts immutable: this foundation does not automatically
rearm the same tuple after a terminal failure. A fresh document produces a new
identity; an explicit request-round/rearm lifecycle must be designed before
activation, together with local preparation and barrier/application ordering. The
[shared request-round follow-on](M7_PREPARATION_ROUNDS.md) now reuses these source
command IDs/receipts for nonce-bound rearm without rewriting history or claiming
application. Initial tuple-only commands remain historical, never relabeled.
Pending rows are outside the existing bounded edge metadata accounting subset.

## Executed evidence so far

Ignored artifacts: `tmp/m7-delivery-preparation-verification/`.

- Sanitized cached Go 1.26.6, `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`,
  `GOSUMDB=off`; no inherited database/provider credentials or Go downloads.
- `focused-04.jsonl`: **98 named race passes, zero failures, 13 MariaDB skips**.
  Includes wire closed-shape/large-decimal tests, genuine encrypted source store,
  exact pending tuple, unchanged regional application, cold duplicate recovery,
  missing/stale/corrupt facts, cancellation, single captured auth and stored session
  change, SQLite issuer/prerequisites/ABA/immutable retry/capability filtering,
  real command dispatch, old/new TLS configuration and request/result lifecycles.
- `session-02.jsonl`: **six named passes, zero failures, three MariaDB skips**.
  The live new session leaves cover regional (no requests), aggregate/both,
  first refused config ACK, old-document policy ABA (no requests), accepted exact
  current report, preparation persistence, first refused hub result, source cold
  reopen and immutable result recovery. Every recorded effective fact stays regional.
- CGO-free `go build ./...`, `go vet ./internal/...`, lint (**0 issues**) after
  cleanup, formatting/diff checks and all **14 gate parser regressions** passed.
  Strict gate now requires **89 M7 markers**, including **13 new** real MariaDB
  preparation markers. Engine skips and parent passes never replace those leaves.
- Five private `.mutations` overlays bypassed the committed report prerequisite,
  current assignment binding, final DB-clock expiry, source regional fact
  prerequisite and one-captured-tuple authentication. Each failed the intended
  assertion, not compilation; unchanged control passed. Production was untouched.

Counts include parents/subtests, not unique functions; separate runs are not additive.
The full gate covers the production/script tree and its tests. The final
core/probe/edge package rerun additionally covers late cipher-replacement, transport
negotiation and pure receipt-recovery assertions. No frontend, Helm,
dependency, database-engine or toolchain change.

### Failed-run ledger

`focused-01` compiled/passed 57 wire cases but repository tests did not build:
a new test incorrectly assumed CompleteCommand returned two values. Actual port
and implementation return one error; caller corrected, without changing the port.
`focused-02` reproduced invalid test assumptions: the stale-owner mutation reused
the real owner ID and emitted a real command, cascading into later row-count
assertions; use a distinct owner. A contradictory result used an unsupported
already_resolved status (validation error), rather than a valid but different
already_applied receipt (conflict). Fix fixtures/oracles, not authority guards.

`session-01` could not dispatch because the older config/report-only harness had
no replay ingest callback: production commands correctly require ingest readiness.
`session-02` adds a real fenced replay store, not a healthy no-op. Refusal waits
now also observe early transport termination. No production readiness check was
weakened or speculative timing retry added. Initial lint caught three ignored test
Close errors and three import groups; cleanup fixed all six warnings.

Manual production-path review considered smaller reuse first: desired document
fields already retain intent, so another desired-state table/diagnostic is not
needed. The command journal supplies request retry/result identity; migration016
adds only the missing source preparation effect. Trace: committed regional report
→ current exact issuer checks → protected command → capability-filtered claim
→ one captured source auth → stored authority/capture comparison → atomic pending
row + receipt → fenced immutable hub result. This is inspection, not independent
engine evidence or aggregate activation acceptance.

### Final executed acceptance and cleanup

Commands executed with the sanitized toolchain described above:

```sh
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-delivery-preparation-verification/dual-engine.jsonl
# Internally: go test -race -count=1 -json -timeout 2400s -p 4 ./...
go test -race -count=1 -json -timeout 600s \
  ./internal/core/domain ./internal/core/services \
  ./internal/adapters/probe ./internal/adapters/repository/edge
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal/ cmd/
git diff --check
go list ./...
```

- Full strict gate: **22 packages, 5,570 named passes, 466 MariaDB passes**, exit0,
  **1,411.1 seconds**; zero failures/MariaDB skips/missing markers/malformed events/
  unrecognized skips. All **89 required M7 markers**, including **13 new**
  preparation markers and real regional/aggregate/both TLS-session leaves, ran.
- The five recognized skips are helper entrypoints, optional live MongoDB/Telegram,
  and isolated disk-full fixture; none substitutes for MariaDB coverage. This is
  not the full section13 failure/operational matrix.
- `final-core-probe.jsonl`: **3,355 named race passes**, zero failures, three
  recognized helper/platform skips. Additional source cipher replacement during
  authentication refuses atomic preparation/receipt writes; pure receipt recovery
  does not load a new config. Real TLS tests prove the issuer is reached only with
  preparation capability and configured dispatcher, observing committed config,
  regional report and fresh post-ACK traffic so silent cases are not dead sessions.
- A **sixth** private mutation disables the transactional captured-cipher comparison:
  `TestDeliveryPreparationCapturedCipherChange` fails its assertion, while unchanged
  control passes (`mutation-cipher-result.json`). No production overlay was applied.
- Final build/vet/lint (**0 issues**)/format/diff/discovery/import checks passed;
  zero Go temporary packages or forbidden production-core imports. Doc links pass.
- Disposable MariaDB container/anonymous-volume cleanup succeeded. Task-owned
  image removed after proving zero users. No global prune, context/config change,
  AWS action, credential change, push/deploy or mode activation. Colima stays running.

Preflight saw three containers, while final inspection saw two. The third was an
unrelated transient container removed **151.9 seconds before this gate started**, as
bounded Docker history confirms (`cleanup-history.json`); it was not the disposable
MariaDB container or removed by this cleanup. Both remaining pre-existing containers
are preserved, and there are zero extra containers. `cleanup-result.json` correctly
retains the raw three-to-two comparison as false rather than claiming all three
still exist. No unrelated container was recreated, deleted or reconfigured.

Historical `make gate-full` vulnerability remediation and migration/load/soak/canary
acceptance remain open. This run neither reruns nor claims the release gate.
