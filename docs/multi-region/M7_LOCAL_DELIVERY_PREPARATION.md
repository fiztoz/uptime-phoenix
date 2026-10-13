# M7 protected local delivery preparation

Bounded continuation after `6bc0752`. **Implemented and verified** as protected
local pending-request persistence. This is not delivery-mode activation or release
acceptance.
Remote preparation remains described in [M7_DELIVERY_PREPARATION.md](M7_DELIVERY_PREPARATION.md).

## Contract and scope

Reuse the protected local configuration activation transaction, exact whole-source
hash proof, existing regional facts and periodic/event refresher. Do not route
`local` through the remote command protocol, invent a runtime session/lease owner,
or infer application from desired fields. Ordinary legacy execution is unchanged.

`LocalDeliveryPreparation` is a pending REQUEST record, not an application fact:
exact hub/local probe, monitor, immutable assignment-set revision, generation,
document revision/hash, reserved local stream, desired aggregate/both, and
microsecond UTC database-recorded `PreparedAt`. No command ID, fake operator,
credential/settings graph, source clock authority, expiry permit or activation token.
The tuple is immutable and keyed by local document/member. A future request-round,
expiry/rearm/barrier lifecycle must cover local AND remote requests before activation.

Migration079 adds `probe_local_delivery_preparations` on SQLite and MariaDB,
positive counters, local-only identity/stream, aggregate/both CHECK, exact snapshot
FK with DELETE RESTRICT, and no backfill. MariaDB prepared_at is DATETIME(6);
SQLite uses TIMESTAMP and the reader checks microsecond UTC shape. Explicit migration
downgrade removes pending-only history, never changes execution mode. Re-up does
not recreate old records; normal fresh publication upgrades them. Fresh activation writes requests after whole-source freshness
and regional facts, in the same transaction as the active pointer/receipt. Regional
members produce no request. Failure rolls back everything. Same-document activation
retry returns the prior receipt unchanged and writes/repairs no preparation rows.

A separate narrow `LocalDeliveryPreparationReader` returns only bounded records for
an exact selected active tuple, independently proving whole-source freshness,
committed receipt and matching regional facts. Missing/partial pre-upgrade rows are
unreported; corrupt present rows fail explicitly without repair or partial results.
It is NOT added to `ReadAppliedLocal`: pending request corruption is not execution
or send authority and must not introduce a new heartbeat/provider-I/O gate.

The protected production refresher is wired to this reader. Both refresh fast paths
require complete records for precisely the positive-bound aggregate/both members;
missing/partial rows upgrade via a NEW configuration revision, never backfill the
old document. Empty/regional-only membership needs no request. Nil reader keeps
unconfigured legacy/test callers unchanged; production wiring fails if unavailable.
Concurrency returns an exact selected tuple or conflict, never a mixed/current-row
rebinding. Historical rows stay historical through membership/policy/mode ABA.

No provider/checker I/O in transactions, no new lease-authorized execution write,
no aggregate application capability or evaluator caller, no suppression/send fence,
no HTTP View/UI/dependency/frontend/Helm change. Configuration serialization does
not acquire these records. Current regional effective state remains unchanged.

## Acceptance plan

Focused domain/service mock-port completeness tests; real SQLite/MariaDB activation,
large binding9007199254740993 distinct from document revision, regional/aggregate/both,
cold exact reads, missing/partial upgrades, immutable retry/no repair, cancellation,
corrupt tuples/receipt/facts with redaction, rollback, stale source/ABA, mode changes
in both directions, concurrent activation/reads, schema up/down/FK/check constraints.
Production refresher cold upgrade and existing recording/provider DOWN/recovery.
Add exact required MariaDB leaves to the strict gate; parent/SQLite passes and
MariaDB skips never establish engine acceptance. Run build/full race/vet/lint,
parser/format/import/package/doc checks and private mutation controls.

Verification artifacts: `tmp/m7-local-delivery-preparation-verification/`.
Historical release vulnerability/operational gates remain separate.

## Executed verification so far

Sanitized cached Go 1.26.6 (`GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`,
`GOSUMDB=off`); no inherited database/provider credentials, modules or toolchain
downloads. Fixture credentials stay private; the strict gate creates its own DB.

- `focused-01.jsonl`: 79 named race passes, zero failures, 37 MariaDB skips.
- `focused-02.jsonl`: **83 named race passes**, zero failures, **38 MariaDB skips**.
  Later run adds worker-before-assignment and both refresh-fast-path tests and
  bounded cancellation/join in the concurrent source/activation fixture. Skips are
  not actual-engine acceptance. Named counts include parents/subtests; runs are
  separate, not additive.
- All **14 strict parser regressions** passed. The gate requires **107 M7 markers**,
  including **18 new exact local preparation MariaDB leaves**. Parent/SQLite passes
  cannot substitute for the expected InnoDB leaves.
- CGO-free build, vet, lint (**0 issues**), formatting, diff, package discovery
  (no temporary packages), and parsed production-core import checks passed.
- Five private `.mutations` overlays omit request persistence, skip the regional
  prerequisite, ignore receipt identity, ignore immutable set binding, or bypass
  both refresh completeness checks. Every mutant fails its intended assertion,
  never compilation; unchanged control passes. Production source is untouched.

## Final executed acceptance

Commands executed (sanitized cached toolchain described above):

```sh
go test -race -count=1 -json -timeout 600s \
  ./internal/core/domain ./internal/core/services ./internal/adapters/repository \
  -run 'TestLocalDeliveryPreparation|TestLocalRegionalApplication|TestPreparedProbeConfigContract'
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-local-delivery-preparation-verification/dual-engine.jsonl
# Internally: go test -race -count=1 -json -timeout 2400s -p 4 ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
gofmt -l internal/ cmd/
git diff --check
go list ./...
```

Full strict gate: **22 packages, 5,639 named race passes, 485 MariaDB passes**,
exit0, **1,491.4 seconds**. Zero failures, MariaDB skips, missing required markers,
malformed events or unrecognized skips. All **107 M7 markers**, including **18 new**
local preparation leaves, execute on InnoDB. The five recognized optional/helper/
platform skips remain separate: group-cycle/crash-child helpers, live MongoDB,
isolated disk-full, and live Telegram. Counts include parents/subtests, not unique
functions; no separate runs are added to these totals.

The full gate covers the final production/test/script tree. Subsequent changes are
documentation only. No failed compilation/startup/fixture/full-gate run occurred in
this slice; retained focused runs do not claim MariaDB coverage. Private mutation
failures are deliberate assertions, not production defects or build failures.

Task-owned MariaDB container/anonymous volume removed by the gate. Task-owned
pinned image removed only after proving zero users. Both pre-existing containers
preserved, zero extra containers; isolated Docker config, existing Colima settings
untouched, daemon left running. No global prune, external database, AWS, credential
change, frontend/Helm/dependency/toolchain change, push/deploy or mode activation.
Cleanup evidence: `cleanup-result.json`. Artifact files are local/ignored, not a
fresh-checkout prerequisite.

Historical `make gate-full` vulnerability remediation and failure-matrix/load/soak/
canary acceptance remain open; the release gate was not rerun or claimed here.

## Production-path review and explicit limits

Smaller reuse considered first: desired snapshot fields already retain intent;
another desired-state or diagnostic table would add nothing. Use fresh activation
for the missing durable local request effect instead of remote wire/command/session
fabrication. A separate reader is necessary because request consistency is NOT a
new execution requirement. It repeats the full graph proof during refresh rather
than letting an earlier observational read vouch for a later capture (bounded extra
read/encoding cost, no performance/load claim).

Trace: protected bootstrap reader wiring (fails if missing) → periodic/event refresh
→ whole-source hash/installation/expected-pointer verification → active receipt and
regional state → pending requests in that SAME transaction. Cold refresh → execution
proof → exact active/receipt/current graph/regional prerequisite/request capture
→ bounded completeness → either stable return or NEW revision publication. Both
fast paths consult completeness. Same-receipt retry returns before writes and never
repairs corruption; corrupt present preparation refuses refresh, while heartbeat,
escalation and provider pre-send remain on their existing execution paths and never
read preparation rows. Legacy dispatch, default/opt-in HTTP DTOs and wire schemas
are unchanged. This is manual inspection, not a separate independent review or
engine execution claim.

The local request tuple has no expiry/round. Remote terminal failures likewise
still need explicit rearm. Before any activation, define a joint request-round,
expiry/rearm, preparation/barrier/application lifecycle; all assigned local/remote
members remain required, including paused/offline members. Configuration activation
is NOT delivery-mode activation. Neither pending request nor regional fact supplies
ModeApplicationSupported, current ownership, quiescence, suppression or provider-I/O
permission. Aggregate incidents/intents/dedup/throttle, final transaction/provider
fences and in-flight crash handling remain unimplemented. Snapshot retention is
restricted by retained local requests; an authenticated request-history/retention
policy remains operational follow-on work. No release/load/soak/canary acceptance.
