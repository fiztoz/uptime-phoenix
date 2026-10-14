# M7 durable aggregate standby reconciliation (no paging activation)

Continuation after `f797ed5`; **implemented and verified** for durable standby
planning only. This is not paging activation or release/operational acceptance.

## Frozen boundary

Reuse EvaluateAggregatePaging/EvaluateMonitorHealth, not materialized display
health, historical replay transitions, preparation requests or source ACKs as
authority. Persist hub-owned STANDBY availability incident identity and held channel
plans in dedicated tables. Neither is an operative/sent incident. No regional
suppression, aggregate provider work, HTTP/UI, source wire/capability or mode
application is enabled. No automatic later promotion of historical held plans.

A joined worker on probes-enabled worker/all nodes claims bounded work with a
separate DB-clock owner/epoch/deadline. Claims commit independently per monitor;
a partial typed error does not discard healthy committed claims. Reconciliation
continues after individual failures and reports joined redacted errors. Every reconciliation locks and checks stored
lease identity and expiry, then captures current complete assignments and accepted
stream/generation evidence in a serializable authority transaction. The current
assignment revision is conservatively adopted on the DB clock inside standby state,
not at the assignment history writer's application-clock `updated_at`. Initial and
changed revisions require subsequent evidence: both source observation and receipt
must be after this persisted adoption boundary; future receipts are excluded.
The extra receipt test is exclusion only, never check truth or a replay transition.
No caller-provided evaluation clock, cached projection or connection liveness supplies
check truth. Observation freshness still has the documented source-clock bounds.
The current state already selects source sequence; historical evidence can only
influence a later independently captured CURRENT snapshot when still fresh.

All assigned members are present; missing, disabled/revoked, retired/reset stream,
stale generation and pre-policy/future/stale observations stay UNKNOWN. Monitor
pause excludes the whole monitor. Maintenance and PENDING never create recovery.
One open standby incident per monitor, UUID identity, monotonic transition versions,
immutable opening configuration. Policy/membership/mode ABA closes administratively,
invalidates held plans, and never reopens in the same evaluation.

Plans contain only incident/transition/channel identities, a canonical channel
revision digest and timestamps. No credentials, endpoints, rendered payloads or
source check output is persisted. Dedicated schema allows held/invalidated only;
existing consumers cannot claim them. Down planning/repeat deadline is durable,
not a last-sent timestamp, delivery-attempt throttle or future send permission.
The last-planned timestamp is persisted separately; changing `resend_interval`
recomputes the deadline from it, including zero-to-positive and positive-to-zero.
Each repeat supersedes previous held down plans rather than creating live delivery
work. Channel changes invalidate stale held plans; a new plan requires independent
current DOWN evidence and the planning interval (or a new incident). Recovery
invalidates earlier held down plans; administrative closure creates no recovery plan.

The channel digest binds current provider configuration, selected template content
and per-monitor include-target flag, without exposing those inputs. Its role is
standby identity/deduplication only; a later real provider intent requires separately
implemented authority and channel revalidation.

New hub SQLite/MariaDB081, explicit up/down; no backfill. Destructive downgrade
drops standby history only and changes no execution. Standby tables cascade with
monitor deletion; UUID identities prevent restored counters from aliasing old
history. Lease expiry is checked again AFTER evaluation/writes before commit.
Clock rollback never moves a stored evaluation backward or shortens a live lease.

## Acceptance plan

Both engines: current local/remote lifecycle, ANY/ALL complete quorum, missing/stale/
paused/maintenance/old-generation/stream/future evidence, current after replay, cold
restart identity/dedup/planning deadline, administrative ABA, channel invalidation,
transaction rollback, stale owner/epoch/expiry including lock wait/evaluation expiry,
concurrent workers/writers, worker-before-assignment, schema/down-up/cascade.
Production local heartbeat/provider DOWN and recovery remain regional and unchanged.
No held plan enters existing local/edge delivery claims; corruption remains outside
regional execution. Exact MariaDB gate leaves, full backend race/build/vet/lint/
parser/format/boundary checks and private assertion mutations. The strict gate
requires 21 exact new MariaDB leaves (143 M7 markers total), including `ClockDomains`,
`RepeatSettings`, and `IndependentBatch`. Parent/SQLite passes cannot substitute.

## Implementation and production trace

- `internal/core/domain/aggregate_standby.go`: nonsecret lease/result/channel
  identity types; `internal/core/ports/aggregate_standby.go`: planning-only ports.
- `internal/core/services/aggregate_standby.go`: bounded claims, pure evaluator
  reuse, per-monitor error isolation, joined cancellation.
- `internal/adapters/repository/aggregate_standby.go`: independent DB-clock
  claims, serializable capture/reconciliation and atomic held-only effects.
- SQLite/MariaDB `081_aggregate_standby` up/down: state, incident and intent
  tables. There is no delivery-claim status or automatic migration into an outbox.
- `internal/bootstrap/run.go`: probes-enabled worker/all composition; default
  probes-disabled execution requires no new external service.
- Repository contract/lifecycle/review tests and service tests; strict gate
  markers in `scripts/m6_dual_engine_gate.py` and its parser regressions.

Production trace: joined worker → current desired/open-incident selector →
independent monitor claim → locked stored lease check → complete current graph/
source/channel capture → post-lock DB-clock recheck and revision adoption →
pure paging evaluation → UUID standby incident/held-plan writes and invalidation →
final DB-clock expiry/rollback fence → commit/release. No checker or provider I/O
enters the transaction. Regional recording/delivery never reads these tables.

## Executed verification (2026-10-13)

Sanitized cached Go 1.26.6 environment: `GOENV=off GOTOOLCHAIN=local GOPROXY=off
GOSUMDB=off`; fixture credentials generated privately at runtime. Commands:

```sh
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal/ cmd/
git diff --check
go list ./...
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
go test -race -count=1 -json -timeout 300s \
  ./internal/core/services ./internal/adapters/repository \
  -run 'TestAggregateStandby|TestDeliveryPreparationContract/sqlite/CapabilityStarvation'
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-aggregate-standby-verification/dual-engine.jsonl
# Strict gate internally executes:
# go test -race -count=1 -json -timeout 2400s -p 4 ./...
```

Corrected strict run: **22 passed packages, 5,742 named race passes, 525 MariaDB
named passes**, zero failures, MariaDB skips, missing markers, malformed events or
unrecognized skips. All **143 M7 markers**, including all **21 new exact MariaDB
standby leaves**, executed. Five recognized optional/helper/platform skips remain.
Summary exit 0 / 1,636.0s; launcher exit 0 / 1,640.8s. Counts include parent/subtest
events and separate runs are not additive. This covers the final production/test/
script tree; subsequent changes are documentation only.

Focused final run: 29 named passes / 21 MariaDB skips, exit 0; those skips are not
engine acceptance. The strict run supplies actual engine evidence. Build/vet/
format/diff passed; lint reported **0 issues**; 14 parser regressions passed;
production-core import scan found no framework/driver violations; package discovery
found no temporary overlay packages; changed documentation links resolve.

Seven private assertion mutations disable final lease expiry, conservative revision
adoption, plan persistence, repeat planning, channel invalidation, batch continuation,
or preservation of committed partial claims. All fail their intended assertions;
the unchanged six-leaf control passes. No build/fixture failure counts as detection.
Two initial mutation selectors matched no intended leaf; their outputs are retained
as selection failures, not acceptance, and corrected exact selectors executed.

Artifacts remain ignored under `tmp/m7-aggregate-standby-verification/`, including
`failed-full-01/`, focused logs, `.trace/`, `.mutations/`, mutation results,
strict JSONL/summary/launcher records, static checks and cleanup evidence. These
local artifacts are not fresh-checkout dependencies.

## Review corrections and failed-run ledger

A bounded read-only review identified the application-clock policy boundary,
batch abandonment on one reconcile error, and enabling repeats mid-incident.
The final implementation:
- Persists conservative DB-clock revision adoption separately from assignment
  history; old evidence is never relabeled as post-adoption. `ClockDomains`
  covers ahead/behind assignment-writer timestamps; its mutation opens on old
  evidence and is caught.
- Commits claims independently and returns both healthy claims and typed errors;
  service reconciliation continues on each claim. `IndependentBatch` proves
  persisted healthy effects despite a bad reconcile and a bad/exhausted claim.
- Derives live planning deadlines from persisted last-plan time.
  `RepeatSettings` proves zero-to-positive arming and positive-to-zero clearing.

The independent review preceded these fixes. Final main-agent tracing and executed
tests verify them; this is not a second independent review or a load/soak review.

The first strict run failed only the existing preparation capability-ordering
fixture: preparation creation used DB time while ACK creation used host time,
making the assumed ordering false by about 19ms. The fixture now constructs
`next_attempt_at` ordering directly from the durable preparation timestamp. It
changes neither protected command metadata nor production capability filtering.
The corrected strict gate passes that exact MariaDB leaf.

Other retained discovery failures: wrong Bun `Limit` integer type, selected
`interval` instead of actual `check_interval`, and import-order lint. A private
raw-error overlay identified the schema name; production error redaction remained
unchanged. DB revision adoption requires a later check, so worker-before-assignment
waits through two honest scheduling cycles rather than fabricating immediate truth.
A reused nullable-pointer test oracle retained its previous value after SQL NULL;
a private reset-only counterexample passed, and the fixture resets that pointer.
These are resolved, not active blockers.

## Resource cleanup

Both strict runs used fresh disposable MariaDB with localhost-only random ports.
The gate removed its owned container/anonymous volume. The task-owned pinned image
was removed only after proving zero users; independent image inspection proves it
absent. The combined tag/digest removal reported exit 1 because the second reference
was already gone; raw cleanup records retain that distinction. Both pre-existing
containers remain present, zero extra containers remain, and Colima configuration/
daemon are unchanged. No AWS, push, deployment, toolchain/dependency, credential,
provider-routing, frontend, Helm, runtime mode or capability action occurred.

## Remaining limits

This slice proves standby storage/planning, NOT aggregate paging, send throttling,
source quiescence/application, stronger capability, committed all-source barrier,
provider crash/in-flight ambiguity, metadata history retention/load/soak/canary or
release acceptance. Do not relabel a held record as a successful send or activate it.
