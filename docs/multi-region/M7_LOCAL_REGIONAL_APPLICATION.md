# M7 local-hub regional availability application state

Bounded continuation after `255b18f` and the
[edge regional application seam](M7_EDGE_REGIONAL_APPLICATION.md).
Implemented and verified on `feat/m7-receipt-backed-config` after `255b18f`.
This is a bounded source-local production seam, not aggregate activation or
release acceptance.

## Scope

Persist honest effective **regional** facts for the protected local configuration
execution path in the hub. Desired aggregate/both never changes the producer's
mode. Reuse `ProbeDeliveryModeApplication`, the existing activation transaction,
`ReadAppliedLocal` execution reader, and existing local heartbeat/escalation/outbox
paths. No new port, wire frame/capability, HTTP View, UI, aggregate sender,
suppression branch or cutover caller. Ordinary no-protected-config legacy runtime
and its dispatcher remain unchanged and unreported.

Facts bind installation/probe (`local`), monitor, exact immutable assignment-set
revision, assignment generation, exact document revision/hash, and reserved
`domain.LocalStreamID`. Assignment/document counters remain independent. Absence
preserves regional execution but supplies no application evidence or suppression
permission. These source-local facts are not authenticated cross-source reporting
and never populate stronger application support in the pure cutover evaluator.

## Storage/lifecycle

- Dual-engine migration 077 adds a local-only regional application table with
  positive identities, regional-only mode, reserved stream, snapshot FK, and no
  plaintext configuration, endpoints or credentials. No migration backfill.
- Fresh `ActivateLocal` already re-encodes the complete locked source graph to
  the candidate's exact hash. Build regional facts from that verified graph and
  commit them atomically with pointer/receipt; failed freshness/count/authority
  checks leave no new facts.
- Preserve same-document retry behavior: return the prior receipt unchanged and
  do not synthesize facts from mutable desired rows. Do not widen legacy hashing
  into a new activation fallback.
- `ReadAppliedLocal` still proves the same full graph (normal bound encoding or
  exact whole-graph legacy-zero retry), then validates any present facts against
  its exact document/membership/bindings. Return nonsecret facts on the internal
  `LocalProbeConfigDefinition`, not on any wire/HTTP View. Missing facts remain
  unreported; mismatched present facts fail explicitly and are not repaired.
- The refresher's fast paths require complete positive assignment and regional
  application bindings. A pre-upgrade bound snapshot with missing/partial facts
  remains executable but causes normal publication/activation of a new revision.
  Empty membership requires no per-monitor facts. Old documents/receipts stay
  unchanged; legacy zero bindings are never filled from current revisions.
- Protected local `CommitLocalHeartbeat` validates present application identity
  inside its existing transaction without replacing monitor lease, generation,
  selected-document or sequence fencing. A set-revision change (including a
  policy-only ABA with the same generation) can refuse an in-flight protected
  sample until reactivation; this includes non-incident samples. Corrupt present
  state can similarly refuse the whole heartbeat transaction, not just an alert.
  Missing pre-upgrade facts remain permissive and unreported. LegacyConfig commits
  do not acquire a protected-execution requirement.
- Escalation enqueue and local outbox pre-send reads already use
  `readAppliedLocalTx` / `ReadAppliedLocal`; those reads now validate present state.
  They remain observational checks, not a newly claimed final transactional
  provider-I/O/lease barrier. A stale planned document is never relabeled current.

## Executed verification

Used cached Go 1.26.6 with a sanitized environment, `GOENV=off`,
`GOTOOLCHAIN=local`, `GOPROXY=off`, and `GOSUMDB=off`. No module/toolchain downloads,
new dependencies, frontend changes, deployment changes or AWS resources.

Commands executed:

```bash
go test -race -count=1 -json -timeout 600s \
  ./internal/adapters/repository ./internal/core/services \
  -run 'TestLocalRegionalApplication|TestAssignmentRevisionBindingContract'
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-local-regional-verification/dual-engine.jsonl
# Strict gate internally executes go test -race -count=1 -json -timeout 2400s -p 4 ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal/
git diff --check
go list ./...
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
```

- Focused corrected checkpoint: **38 named passes**, zero failures, **20 MariaDB
  skips** before server provisioning. That was not engine acceptance; later
  stream/mode corruption and cascade cases are covered by the strict full run.
- Corrected strict full gate: **22 packages, 5,285 named passes, 422 MariaDB passes**;
  zero failures, MariaDB skips, missing markers or malformed events. All **48 M7
  markers**, including **17 new exact local regional application leaves**, ran.
- Five recognized skips: group-cycle/crash-child helpers, optional MongoDB and
  Telegram targets, and the dedicated Linux disk-full fixture. This is not the
  full section-13 operational matrix.
- CGO-free build, vet, lint (**0 issues**), formatting, diff, package discovery
  (no temporary packages), changed-document link checks, and **14 gate-parser
  regressions** passed. Only comments/docs changed after the full run began.
- Five private `.mutations` overlays substituted desired mode, omitted fact
  persistence, bypassed fact reads, bypassed heartbeat checks or skipped refresh
  completeness. Every mutation failed intended assertions, not compilation;
  the unchanged control passed. Production sources were never overwritten.
- Bounded read-only production-path review found no blocker. The accepted
  in-flight sample refusal window is explicit above; a speculative migration
  fixture-healer change was not adopted. The observed rollback dependency issue
  was corrected in its actual fixture instead.

Coverage includes large exact set binding **9007199254740993** independent of
config revision, all desired modes with effective regional facts, cold reads,
atomic activation rollback, same-receipt immutability, missing/partial upgrade,
legacy zero preservation, cancellation, redacted corruption refusal, damaged
stream/mode state with schema checks restored before reads, heartbeat persisted
side effects/sequence rollback, actual escalation enqueue transaction refusal
with an otherwise-identical successful control, local outbox pre-send refusal,
mock-provider DOWN/recovery sends, schema CHECKs, snapshot-delete cascade and
migration down/up without fabricated backfill. Existing lease/assignment,
remote/edge/session/API regression suites ran in the full gate.

The first full gate failed in `TestPreparedProbeConfigContract/mariadb/RollbackMigrationBounds`:
its old teardown removed snapshot migration 047 without removing dependent 077.
InnoDB rejected the parent-table drop (1451); SQLite did not expose that empty-table
DDL boundary. The fixture now removes/restores 077 in dependency order. The failed
full run and two focused test-assumption failures remain preserved, not passes.

Fixtures used existing Colima with fresh uniquely named databases/containers and
random localhost ports. Official image digest:
`mariadb@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15`.
Owned containers/anonymous volumes are removed by the gate; the task-owned image
is removed after confirming no container uses it. Existing containers and Colima
configuration stay untouched; no global prune. Local evidence is under
`tmp/m7-local-regional-verification/`, including `failed-full-01/`, focused logs,
strict JSON/summary, checks, mutation results and cleanup record.

## Remaining acceptance and next seam

This local slice adds no authenticated reporting. The follow-on
[regional reporting slice](M7_REGIONAL_APPLICATION_REPORTING.md) implements
truthful remote regional facts with a regional-only capability, not aggregate
application support. Aggregate runtime, committed cutover barriers, queued/in-flight
suppression, durable aggregate incident/intents and UI controls remain unimplemented. Ordinary legacy
runtime remains unreported, and protected local pre-send reads still do not prove
final transactional send authority. Edge metadata accounting remains separate.

`make gate-full` was not rerun: its historical Go standard-library vulnerability
failure remains an independent release blocker. Full failure/load/migration/soak/
canary acceptance and M7 activation checkboxes remain open. Backend slice
verification does not authorize push, deployment or activation.
