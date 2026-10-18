# M8 — one-shot installation resource coordinator

Continuation after local checkpoint `7b38761`. This slice connects the existing
[installation journal](M8_INSTALL_JOURNAL.md) to a synchronous, joined executor
port. It is an internal use case, not an SSH/Docker/systemd adapter, installer
endpoint, startup worker, enrollment protocol or installation-success feature.
There is no concrete remote executor or live opt-in in this slice.

## Frozen interfaces and ownership

New port, `internal/core/ports/probe_install_executor.go`:

```go
type ProbeInstallResourceExecutor interface {
    ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation,
        kind string) (domain.ProbeInstallResourceOutcome, error)
}
```

The operation is a defensive copy of an acknowledged reservation receipt. It
identifies the exact immutable operation/owner, artifact request, canonical
resource, attempt and DB deadline. It is not a transferable authorization token,
proof of current ownership, verified bytes, remote ownership or cleanup authority.
The coordinator calls the port at most once for each newly acknowledged intent.
The port must return only after all work for that invocation has joined. A future
implementation must independently enforce host trust/destination policy, actual
artifact verification, remote isolation/fencing, bounded process termination and
truthful joined reports. Closing a local SSH channel alone does not establish
remote termination. No arbitrary commands, credentials or raw output cross this
port. The caller owns any executor/session/artifact lifetime outside each call.

New service API, `internal/core/services/probe_install_runner.go`:

```go
func NewProbeInstallRunner(journal *ProbeInstallJournalService,
    executor ports.ProbeInstallResourceExecutor) (*ProbeInstallRunner, error)
func (r *ProbeInstallRunner) Run(ctx context.Context,
    expected domain.ProbeInstallIdentity, ownerID string,
    attempts []string) (*domain.ProbeInstallOperation, error)
```

Exactly one canonical, unique attempt UUID is supplied per canonical resource,
in layout order; attempts are copied before I/O. IDs are supplied by the future
admitting composition root rather than introducing a generator dependency. No
Create, takeover, resume, renewal, retries of admission or automatic redispatch.
The journal facade and all stored fencing/schema semantics remain unchanged.
A nil Run error can accompany nonterminal resources_ready, joined canceled, or
failed/not_started: callers must inspect the receipt, never interpret nil as
installation success. not_started describes the current action; earlier applied
effects remain recorded. An error may return the most recent acknowledged,
identity-bound receipt, not a fabricated terminal outcome.

Main owns this contract/port, new real-store integration tests, gate/parser and
documentation. A service worker owns ONLY the new runner service and its unit
tests. Existing journal, M7, preflight, startup, HTTP and schema files are not
owned by this slice. No external dependencies or migrations are added.

## Execution and authority

1. Validate the context, expected identity, owner and complete attempt list before
   I/O. Observe the immutable identity/owner before Begin; mismatches never begin
   or dispatch. Revalidate every acknowledgement against the expected identity.
2. Begin exactly once, with a bounded metadata context. Only an acknowledged
   queued-to-running transition permits this invocation to own the sequence.
   A failed/lost Begin acknowledgement never dispatches, finishes or interrupts
   somebody else's potentially winning run; cold expiry remains honest.
3. Reserve each canonical resource once, in order. Only `created=true` with the
   exact in-flight receipt can invoke the executor. `created=false`, failed/lost
   acknowledgement or malformed receipts never invoke or continue from history.
4. Re-observe after reservation and before invocation. Known cancellation,
   terminal/expired state, changed identity/owner/attempt or unavailable storage
   stops dispatch. This is observational defense, not a remote transactional
   fence. A future adapter still owns the last remote mutation guard; no DB
   transaction spans external I/O.
5. Invoke synchronously on a bounded cancelable context, with a defensive copy.
   Persist the joined report on a separate bounded metadata context. Never reserve
   the next action before the predecessor report has committed successfully.
   A failed/lost report acknowledgement stops the sequence without redispatch.
6. Only joined, valid reports with nil executor error are trusted. A nonnil error,
   invalid/empty report or ambiguous outcome is `unknown/outcome_unknown`, never
   silently `not_started` or applied. A definitely joined applied report remains
   applied even if cancellation raced its acknowledgement; it does not authorize
   more work. If the coordinator knows it never called the port, not_started is
   truthful. Executor errors/remote output never escape in public errors.
7. All applied reports yield nonterminal `resources_ready`, never installed or
   success. No executor remains running when Run returns. The coordinator does
   not keep a worker alive after returning readiness; a future enrollment/config
   controller must define the rest of the operation lifecycle.

## Cancellation, expiry and joining

After acknowledged Begin, a per-run watcher observes storage at most once per
second with two-second read contexts. Durable cancel_requested, terminal/expired
state, identity/owner changes, or unavailable/corrupt observation cancel the
in-flight action. A process-monotonic 15-minute upper bound also limits a run;
the caller's shorter cancellation/deadline is honored. Database authority is
judged by the journal/DB clock, never by comparing host time to DB timestamps.

The executor must respect cancellation and join its work before returning. The
coordinator waits for that return and joins its watcher before terminal outcome
writes or return; it cannot forcibly make an uncooperative port terminate and
must never invent a joined outcome. Test that a deliberately held invocation
prevents terminal cancellation and Run return until explicitly released.

Durable cancellation intent precedes terminal canceled. A joined applied or
not_started result can be recorded under cancel_requested, then the owner may
finish canceled when no uncertainty remains. Unknown reports atomically interrupt
and stop the graph. Caller shutdown/deadline or observation loss is owner
interruption, not invented user cancellation. Never clear durable uncertainty,
overwrite cold deadline_expired state or continue dependents after cancellation.
When final metadata writes fail, return a fixed error and leave persisted held
intent for cold reconciliation; do not fabricate terminal success locally.

Every metadata call has a bounded five-second context (watcher reads: two seconds).
Final recording/observation/unsuccessful finish use independent bounded contexts,
not an already-canceled caller. No unbounded retries. Complete operations returned
by the service own their slices, and all errors preserve only fixed domain/port/
context sentinels. No private executor or storage diagnostics are returned.

## Required verification

Unit tests use mocked journal ports and a scripted executor, with no DB/network
I/O. Cover input/identity/owner checks before Begin, two competing runs, exact
receipt binding, fresh-versus-historical grants, lost acknowledgements, context
revocation at every handoff, malformed/private errors, no dependent dispatch,
defensive copies, watcher cancellation/joining, independent bounded completion
contexts and honest clock behavior. Tests may use shorter per-instance internal
poll intervals; production defaults are fixed, not configurable globals.

Real-store tests live in NEW
`internal/adapters/repository/m8_install_runner_test.go`. Required MariaDB leaves:

- `TestM8InstallRunner/mariadb/ResourcesReady/systemd`
- `TestM8InstallRunner/mariadb/ResourcesReady/docker`
- `TestM8InstallRunner/mariadb/ConcurrentRunSingleBegin`
- `TestM8InstallRunner/mariadb/LostReservationAckNoInvocation`
- `TestM8InstallRunner/mariadb/RemoteCancelJoinsBeforeTerminal`
- `TestM8InstallRunner/mariadb/OwnerLossUnknownStopsDependents`
- `TestM8InstallRunner/mariadb/FinalAppliedOwnerLoss`
- `TestM8InstallRunner/mariadb/ColdExpiryRejectsLateApplied`
- `TestM8InstallRunner/mariadb/MalformedOutcomeCannotAdvance`
- `TestM8InstallRunner/mariadb/IdentityOwnerNoDispatch`
- `TestM8InstallRunner/mariadb/ObservationLossStopsDispatch`

Dispatch callbacks read from an independent connection to prove committed
in-flight intent and applied predecessors. Cancellation tests assert raw stored
intent while a call is held, then joined terminal state; cold expiry tests verify
late applied cannot repair unknown. Preserve no-grant lost-ack effects and no
invocation on retry/cold restart. Use only new disposable local MariaDB resources.
Keep the original 34 journal leaves mandatory and unchanged; a dedicated runner
scope requires both sets (45 total), no skipped/missing/malformed coverage.

Negative overlays must fail intended assertions, not compilation/timeouts/panics.
Run full default race, build/vet/format, CGO-free Linux builds, parser/shard
regressions and the strict real-engine scope on frozen source. Inspection and
worker reports are supplementary, not executed acceptance.

## Corrections found during handoff verification

- **Last applied owner loss:** the draft checked cancellation on the next loop
  iteration, but the last resource returned through a readiness fast path with
  no next iteration. A joined executor that canceled its caller and returned
  definite applied preserved the effect but incorrectly escaped interruption.
  `handoff-repro-01` reproduced first-position success/last-position failure.
  Readiness now checks known stop and closes out through the existing journal;
  tests cover both positions, plus actual persisted all-applied interruption on
  SQLite/MariaDB. This does not turn applied history into not_started.
- **Final acknowledgement identity:** the facade binds operation ID/owner/outcome,
  not the runner's entire expected request. A valid mock Finish receipt with a
  different actor passed those narrower checks. `foreign-finish-repro-01` failed;
  the detached Finish helper now compares the complete immutable identity and
  returns conflict without surfacing that receipt. This is a port-boundary
  regression, not a demonstrated concrete-store identity mutation.

The first/last regression and final acknowledgement control passed three times
(`handoff-control-02`, 12 named passes). Earlier draft failures are retained, not
rewritten as successful runs. `review-ledger.json` records the mechanisms and
scope; read-only review/worker reports do not establish executed acceptance.

## Evidence and limits

Committed locally as **`0870c3a`** after journal checkpoint `7b38761`; not pushed.
The source and documentation matched the evidence below before committing; no
verification was rerun for that commit-only operation.
Frozen source is
`3a215986cf75757850b63bf168086998ac5e0b33ddba6088b4a15e47015bb659`
(1,464 files). Private evidence lives under
`tmp/m8-install-runner-verification/`.

Executed on Go 1.26.9 with offline modules/toolchain and isolated task scratch:

| Gate | Result |
|---|---|
| Focused journal + runner race, three repetitions | 636 named passes, 156 MariaDB pass events; zero failures/skips; all 45 required leaves passed three times |
| `--scope m8-install-runner`, fresh disposable MariaDB 11 | 212 named passes, 52 MariaDB pass events; all 45 required leaves; zero failures/skips |
| `go build ./...`, `go vet ./...` | Passed |
| Separate CGO-free Linux amd64/arm64 builds | Passed; binaries not executed |
| Parser/scope / repository-shard regressions | 27 / 17 passed |
| `gofmt -l internal/ cmd/`, whitespace check | Empty/pass |
| Assertion mutations | 12 caught; no compile/panic/race/timeout substitutes |
| Full default `go test -race -count=1 -json -timeout 2400s -p 4 ./...` | 5,937 named passes, 23 passing packages; zero failures; 547 named skips; 1,908.007 seconds |
| Untouched-source post-mutation race control | 212 named passes, 52 MariaDB pass events; zero failures/skips |
| Cleanup | Exact task-owned DB/volume, generated credentials and scratch removed; two baseline containers and cached image preserved |

Mutation recipes cover initial identity observation, fresh-only grants,
pre-invocation observation, malformed reports, independent completion context,
last-applied cancellation, final acknowledgement identity, joined invocation,
report acknowledgement before dependents, attempt copies, bounded recording,
and valid-applied-plus-executor-error uncertainty. The last recipe asserts both
SQLite and MariaDB persisted unknown with no dependent dispatch; the other
recipes exercise the coordinator through mocked journal ports. Original source
is untouched: Go replacements live only in dot-prefixed overlays. The final
post-mutation control reran the complete journal+runner selection on both engines.
Prior journal
mutation evidence belongs to its earlier checkpoint, not this new count.

The full default run intentionally had no MariaDB DSN: **542** named cases
skipped for that missing prerequisite, plus five existing optional/helper cases
(`TestDatabaseChecker_Check_MongoDB_RealServer`,
`TestTelegramSender_Send_DownSeverity`, `TestEdgeCheckCrashChild`,
`TestEdgeDiskFullCriticalCommit`, `TestConfigGroupCycleHelperProcess`).
There were no unexplained skip reasons or malformed events, and 14 packages had
no test files. Default-suite MariaDB-named parent passes are not engine coverage;
the separate strict scope, repeat and final control establish journal/runner
real-engine coverage only. `coverage-audit.json` records this distinction.

`verification-result.json` and `final-audit.json` bind final acceptance to the
unchanged source hash/inventory. `cleanup-result.json` records exact task labels,
joined users and preservation checks; no task DB, generated credential or scratch
cleanup remains outstanding. Failed draft reproductions, commands, logs,
manifests and mutation overlays remain private under the evidence directory.
No push, deployment or installation followed `0870c3a`. The follow-on
[image-content prerequisite](M8_IMAGE_CONTENT.md) is separate from this coordinator
and adds no concrete remote executor or installed-success outcome.

`golangci-lint` and `govulncheck` are unavailable; vet/format are fallback evidence,
not a completed lint or vulnerability scan. No frontend or Helm source changed;
those gates were not run. Scoped real-engine passes do not establish broad M7
MariaDB acceptance. Real image correspondence and layer/publisher
verification, remote artifact delivery/host ownership/fencing, joined remote
termination, cleanup, admin async installer API, redacted progress, authenticated
runtime enrollment and exact config activation remain open. So do broad M7
MariaDB acceptance and M6/M8 operational/release/deployment gates. No push or live
installation is authorized by this internal coordinator contract.
