# M7 operative aggregate/both cutover

## Status and scope

This is the operative implementation following `919ffd2`, authorized behind an
explicit opt-in. It is distinct from the historical standby-only checkpoint.
The [implementation contract](M7_OPERATIVE_CUTOVER_CONTRACT.md) records the work
boundary; this document records runtime semantics and verification limits.

**Backend implementation accepted for PR, 2026-10-14.** The strict full
SQLite/disposable-MariaDB race gate passed on the unchanged implementation tree.
This accepts the default-disabled backend slice, not deployment, live paging
activation, vulnerability remediation or the remaining operational release gates.
The source gate/provider journal deliberately has no ambiguity override.

## Opt-in and compatibility

- `PROBES_AGGREGATE_PAGING_ENABLED` defaults to `false`. True requires
  `PROBES_ENABLED` and the existing protected probe composition/key configuration.
- Existing desired `regional` / `aggregate` / `both` persistence is not itself
  runtime authority. Existing HTTP diagnostics, default/false responses and PUT
  behavior are unchanged. There is no new UI mode selector or application control.
- Standby incidents and held plans remain separate; none is promoted into a send.
- Ordinary regional execution is not preparation-gated. Missing regional facts
  remain compatible; present contradictory facts fail closed. Legacy acknowledged
  local whole-source execution remains supported without synthesizing bindings.
- Certificate, capacity and probe-connection delivery remain independent of the
  availability gate. Check/incident truth continues while availability paging is
  suppressed. BOTH retains source regional paging; AGGREGATE suppresses it.

## Committed lifecycle

The joined hub worker uses stored DB-clock owner/epoch/deadline claims. Each
monitor is independently processed; one refusal does not discard another
monitor's committed work.

1. **PREPARING:** independently verify the complete current preparation round,
   immutable membership, exact protected acknowledged documents, set bindings,
   generations, accepted streams, regional facts and actual strong capability.
2. **QUIESCING:** persist a new UUID barrier and its original complete membership,
   then issue protected `delivery.quiesce` commands. A source writes its hold
   before checking for provider sends still in progress, in the same
   transaction. A busy
   refusal is not quiescence, and it does not leave the hold engaged: the
   barrier cannot activate while that source refuses, so a kept hold would leave
   the monitor with no page path at all (#191). The edge keeps the refused gate
   as a non-suppressing `quiescing` marker; the hub-local source withdraws it.
   The hold suppresses regional availability work only from a successful quiesce.
3. **QUIESCED:** advance only after all exact quiesce successes are committed.
4. **APPLYING:** issue `delivery.apply` against the exact quiesced gate and captured
   document. Persist source application and immutable kind-specific receipt.
5. **ACTIVE:** advance only after all exact applications commit. Genuine aggregate
   incidents, transition/recovery/repeat intents and delivery claims use separate
   operative tables and the existing pure paging/cutover evaluators.
6. **DRAINING:** disable, membership/document/policy/generation/stream drift, or
   the activation deadline (below) stops admission and supersedes safe unstarted
   plans. Admitted hub provider work that may still be running blocks resume
   (see settled attempts below). Only exact original-member `delivery.resume`
   successes release the source holds.
7. **RELEASED:** a subsequent deliberate opt-in needs a new barrier. After a
   deadline drain the released state keeps `cutover_deadline_exceeded` and the
   monitor waits out the re-entry backoff before preparing one. Local
   application history includes barrier identity, so the same retained document
   can be applied in a later cycle without aliasing or rewriting the earlier fact.

Paused, disabled, offline, missing and removed-but-not-resumed sources do not
shrink the barrier membership. Expiry, reconnect, reset and configuration
replacement cannot erase a source hold. Resume-before-quiesce leaves an exact
released tombstone, preventing a delayed old quiesce from reinstalling a hold.
Blocked established barriers remain discoverable for explicit disable/drain.

### Activation deadline and re-entry backoff (#209)

A quiesced member suppresses its regional availability paging until its barrier
drains. A barrier therefore may not wait indefinitely for its slowest member:

- **Deadline.** A barrier that has not reached ACTIVE within
  `operativeActivationDeadline` (5 minutes, DB clock, from the barrier's
  creation) drains with reason `cutover_deadline_exceeded`. This covers
  QUIESCING, QUIESCED and APPLYING, and a pre-active BLOCKED barrier whose reason
  is a member refusal or missing evidence (`quiesce_refused`, `apply_refused`,
  `cutover_evidence_incomplete`). Corrupt-evidence blocks
  (`barrier_identity_conflict`, `barrier_evidence_conflict`, `missing_barrier`)
  still wait for explicit disable/drain, because their tuples are not trusted.
- **Drain.** The ordinary drain path runs: unstarted plans are superseded, every
  original member is sent `delivery.resume`, and each source releases its own
  hold when its resume commits. An offline member's resume waits for it, but it
  no longer holds anyone else.
- **Re-entry backoff.** A monitor released by a deadline drain does not prepare a
  new barrier until `operativeReentryBackoff` (15 minutes) has passed since that
  barrier's last resume. The wait doubles for each consecutive barrier of the
  monitor that never activated (up to 8 are counted), capped at 24 hours. A
  permanently refusing or offline member therefore costs its peers one bounded
  window per cycle, with cycles growing further apart.
- **Visibility.** The transition logs one error-level line with the monitor,
  barrier, the phase it was stuck in and its age. The persisted state keeps the
  reason through release until the next barrier starts.

### Source session lapse while ACTIVE (#213)

A hub restart lets a source's session lease expire; a transient hub or
database error closes the probe connection and releases its lease; or the
source reconnects at a later connection generation (every dial takes one,
including dials that fail while the probe is down) before its handshake
re-attests the cutover capability. Neither is lost evidence: the recorded capability is intact and the
source hold persists. An ACTIVE barrier whose evidence is incomplete *only*
because of such a lapse stays ACTIVE for up to `operativeSessionLapseGrace`
(3 minutes, DB clock, measured from the expired `lease_until`, or from the
session's `last_contact_at` when the hub released the lease). While held it
admits no aggregate paging; the open aggregate incident is kept, so the
recovery page comes from the same incident once the source re-attests. Without
this the barrier drained (`cutover_evidence_incomplete`), the drain closed the
incident as `admin_closed`/`drained` with no recovery page, and the replacement
barrier opened a new incident and paged DOWN again.

The hold is bounded: a lapse older than the grace, a released session with no
recorded contact, a withdrawn capability, or any other incomplete evidence
(revoked or deleted probe, retired stream, pending stream reset) still drains
as before. Trade-off: a source that truly
died with the hub leaves the monitor without aggregate pages for at most the
grace before the drain resumes regional paging. Activation and provider
authorization still require a live, attested session; the hold confers no
send authority.

Disable is not regional resume. Startup refuses removal of protection/runtime
needed by durable authority, and migration downgrade refuses unresolved authority.
Do not edit the provider journal to invent success or manufacture a historical
completion timestamp.

### Settled provider attempts (#211)

The cutover checks wait for a provider attempt only while it can still be
doing provider I/O: `in_flight` and inside its budget plus a settle grace. On
the hub that is `budget_until` + 1 minute on the DB clock; on a probe it is the
grant time + the 30-second authorization/send budget cap + 1 minute. The grace
covers the five-second completion write and clock skew.

An `ambiguous` attempt (the provider returned an error) and an `in_flight`
attempt past that window (a crash or a lost completion write) never resolve.
They used to be counted across the monitor's whole history, so one failed send
blocked drain forever: every member stayed held, aggregate paging was stopped,
and the monitor paged nobody. Every escape also goes through drain. One old
failure also refused every later quiesce, so the monitor could never cut over
again. Now they block nothing:

- barrier creation, every source's quiesce and drain's resume proceed;
- their journal rows are kept exactly as recorded, never rewritten or marked
  succeeded;
- when a barrier releases with ambiguous attempts admitted since it opened, the
  hub logs one warning with their count, because a regional page after resume
  may duplicate one of them.

A possible duplicate page is accepted in place of an unpaged monitor. A clock
step forward can settle a live attempt early, with the same bounded duplicate
risk; a step backward only makes the wait longer.

## Provider authority

Final provider authorization independently rechecks the exact stored claim,
current complete barrier/application membership, protected capture, lifecycle,
channel/template/target-policy digest, accepted sessions and budget in one DB
transaction. Claim selection, cached selectors and historical receipts are not
send permission. No DB lock spans provider I/O.

Plans store identity and digest metadata, not plaintext notification settings.
The sender and renderer use the exact settings returned by final authorization,
not an earlier observational read. A changed channel can safely invalidate
unstarted plans even when it also makes the acknowledged whole-source graph stale;
that refusal never returns a provider grant.

One bounded deadline covers authorization and provider work. Each actual start
has an opaque UUID attempt journal. Only the caller holding that exact returned
attempt can complete it:

- Provider return without error records `succeeded`.
- Provider error records `ambiguous`.
- A genuinely pre-I/O refusal records `not_started`.
- Crash/unfinished work remains `in_flight`; no timeout, lease reclaim or restart
  proves whether external I/O occurred.

Completion uses an independent five-second context after provider return. It is
identity-authorized, including after lease expiry or a backward clock step.
`finished_at` records the actual database observation; it is not fabricated or
clamped to claim time and must not be interpreted as exact external I/O timing.
Contradictory terminal outcomes are refused. There is no exactly-once external
notification claim: provider acceptance and local persistence are separate.

### Aggregate queue recovery after lost completion (#213)

An expired queue lease is not permission to repeat an admitted provider request.
Before incrementing the claim counter, the aggregate claim transaction examines
its durable journal and intent under the database lock:

- A grant still inside its budget plus settle grace is skipped without blocking
  other deliveries.
- A persisted `succeeded` grant settles a lost queue receipt as `sent`, without
  sending again.
- An expired unfinished grant, or an `ambiguous` grant without a committed retry
  decision, terminates the queue row as `failed / provider_outcome_unknown`.
  This is a queue decision, not proof that the provider rejected the request.
- Only explicit, still-current `retrying` work retains the existing five-attempt
  bound. A claim with no grant, or a proven `not_started` grant, is reclaimable.
- Obsolete retries (drain, recovery or a superseding transition) are retired;
  their already-finalized intent is never rewritten. Recovery work remains a
  separate delivery and can proceed past the retired DOWN.

Recovery never rewrites a crashed journal row to `ambiguous`, invents
`finished_at`, or fabricates success. Its `outcome_at` is the DB observation of
queue reconciliation. Existing late exact journal completion remains valid,
while a stale queue receipt cannot rearm the retired delivery. No schema,
source hold, final provider authorization or regional retry contract changes.

The regression tests simulate both crash/lost-write boundaries through cold
stores and consumers, including competing consumers and another channel's work.
They do not reproduce the original four-remote-probe SIGKILL campaign. A drain
caused by missing session/application evidence still closes administratively;
this fix does not turn such a drain into a health recovery or weaken its fences.

**Verification, 2026-11-04.** The final
`go test -race -count=3 -json -timeout 5m ./internal/adapters/repository -run
'TestOperativeAggregate(Restart|ReceiptLoss|BoundedRetry)$'` run passed all 13
required cases on each engine, three times: 99 named passes including parent
nodes, 45 MariaDB named passes, zero failures/skips. Build, CGO-free build,
`go vet ./internal/...`, gofmt and whitespace checks passed. `golangci-lint`
was unavailable; vet/gofmt were the documented fallback. The dual-engine/parser
regressions passed 46 tests and repository-shard regressions passed 17.

Earlier attempts are not counted as passes: host/container clock differences
exposed pre-activation observation and pre-lease fake-return assumptions in the
new fixtures; the fixtures now wait for the real persisted boundaries without
weakening production checks. One earlier repeated run also had a single
unexplained authorization refusal; it did not recur in 20 targeted diagnostic
runs or the final full focused run. Temporary diagnostics were removed.

Broader verification is **not green**: an earlier native full race run reached
its repository package's 40-minute timeout during unrelated migration setup
(23 packages passed; 10,759 named passes; 598 skips, including 577 MariaDB skips).
The broader operative dual-engine run had 12 failing MariaDB leaves; five
`TestOperativeCutoverContract` failures were reproduced on unchanged `021f005`
before the modified claim code is reached. The other seven were not separately
baseline-verified. Neither partial run establishes whole-backend acceptance.
Task-owned temporary MariaDB containers were removed. No live deployment,
provider endpoint, source hold or original UAT campaign was changed.

The local final grant also proves its stored availability deadline against the
DB clock, so a caller clock ahead cannot authorize future queued work.
Migration 082 conservatively observes already-attempted legacy local availability
work as ambiguous when completion cannot be proven. Migration timestamps describe
that observation, not reconstructed past provider start/finish times.

Repeat uses persisted admitted-intent history and the current resend interval,
not fabricated last-sent time. UNKNOWN/PENDING/maintenance do not fabricate a
recovery or repeat. Recovery binds the exact resolved/recovered transition and
projects UP. Administrative changes cannot reopen an old transition through ABA.

## Wire and composition

The exact strong hello capability is `availability.delivery_cutover.v1`, advertised
only with the compiled durable executor, source gate and provider journal. The
welcome's optional `delivery_cutover` is a strict boolean; absent means false.
Regional reporting and preparation capabilities are not substitutes. Resume
remains deliverable while the hub opt-in is disabled and draining.

Protected command kinds are `delivery.quiesce`, `delivery.apply`, `delivery.resume`.
Each carries the exact preparation/application tuple plus `barrier_id` and
`preparation_command_id`. Positive counters retain canonical signed-int64 decimal
string encoding and independent assignment/document revision spaces. Success
codes are respectively `delivery_quiesced`, `delivery_applied`, `delivery_resumed`;
code-empty or mismatched success is invalid. Retries recover immutable exact
historical receipts, not current eligibility.

Hub capability storage proves the accepted stored session after committed traffic
(telemetry or authenticated regional report), not merely after writing welcome.
The command service explicitly decodes/wires these kinds through the production
codec. Both hub and edge workers are joined on shutdown.

Key files:

- `internal/core/domain/operative_delivery.go`, `internal/core/ports/operative_delivery.go`
- `internal/core/services/operative_delivery_service.go`, `provider_attempt.go`,
  `delivery_consumer.go`, `edge_delivery_service.go`, `probe_command_service.go`
- `internal/adapters/repository/operative_*.go`
- Hub SQLite/MariaDB `082_operative_cutover.{up,down}.sql`
- `internal/adapters/repository/edge/{delivery_cutover,provider_attempt}.go`, edge 017
- `internal/adapters/probe/delivery_cutover_*.go`, transport/runtime integration
- `internal/bootstrap/{config,run}.go`, `cmd/probe/runtime.go`

## Verification ledger

Commands executed on the final implementation tree:

```text
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal cmd
git diff --check
python3 -B scripts/test_m6_dual_engine_gate.py
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image <pinned official digest> \
  --go-json tmp/m7-operative-cutover-verification/dual-engine.jsonl
# Gate executes: go test -race -count=1 -json -timeout 2400s -p 4 ./...
```

The private launcher selected cached Go 1.26.6 with `GOENV=off`,
`GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, durable task-owned TMPDIR and no
inherited DB/provider credentials. The database image was exactly
`mariadb@sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15`,
with isolated Docker configuration, localhost-only random port and no host mounts.

| Evidence | Executed result |
|---|---|
| CGO-free build / vet | Exit 0 / exit 0 |
| golangci-lint | Exit 0, zero issues |
| gofmt / diff check | Empty format output / exit 0 |
| Gate parser regressions | 14 tests passed |
| Full race packages | 22 passed |
| Named race passes / failures | 5,976 / 0 |
| MariaDB named passes / skips | 570 / 0 |
| Required markers | All 203, including all 181 exact M7 leaves |
| Malformed JSON / unexpected skips | 0 / 0 |
| Go elapsed / outer elapsed | 1,913.4 s / 1,917.1 s |
| Source fingerprint | All 1,362 Go/SQL/Python inputs unchanged from gate launch |
| Forbidden core framework/driver imports | None found |

Five assertion mutations were caught with a passing unchanged control:
availability DB deadline, clock-independent exact completion, source suppression,
same-document barrier identity and completion-state integrity. None was a
compilation or zero-test failure. Mutants and the control ran on SQLite and are
supplementary to, not replacements for, the full production-engine gate.

Real encrypted-edge / pinned-TLS sessions reached ACTIVE and then RELEASED in
both modes on both hub engines. Executed operative leaves include complete source
membership, ambiguity/drain, provider success/error, DB-clock claim fences,
startup/downgrade and legacy upgrade guards, same-document re-enable history,
ordinary missing-fact/preparation compatibility, queued phase withdrawal,
standby/operative isolation, fresh repeat/throttle and channel-digest refusal.

The first full real-engine run failed before operative coverage completed.
MariaDB revealed the aggregate journal scope column was one character too short;
this was corrected rather than inferred from SQLite success. Timing traces also
showed genuine producer samples 10–20 ms ahead of the disposable DB: existing
standby fixtures now wait for the authoritative clock without rewriting evidence.
Replay fixtures wait before ingest because production correctly archives future
observations without making them current. Worker-before-enrollment fixtures wait
for committed adoption and emit one stable genuine sample instead of repeatedly
racing fresh writes against the reader.

The full run had five recognized **non-MariaDB** skips:
`TestConfigGroupCycleHelperProcess`, `TestDatabaseChecker_Check_MongoDB_RealServer`,
`TestEdgeCheckCrashChild`, `TestEdgeDiskFullCriticalCommit`,
`TestTelegramSender_Send_DownSeverity`. They do not establish their unexecuted
coverage. There were no missing required markers or skipped MariaDB tests.
Focused runs and the failed first full run remain preserved, not added to these
counts or retrospectively called passes.

Private ignored evidence lives in `tmp/m7-operative-cutover-verification/`:
`summary.json`, `gate-exit.json`, `dual-engine.jsonl`, `source-fingerprint.json`,
`static-summary.json`, `*-06.log`, `.mutations/`, `failed-full-01/`, and
`resource-cleanup.json`. These artifacts are local evidence, not checkout/runtime
prerequisites and are not published with the PR. No DSN or credential is printed.

Cleanup independently confirmed the two pre-existing containers preserved, no
extra containers or task-owned volumes, zero users of the task-owned image and
the image absent after removal (exit 0). The Docker daemon remains running.
No deployment, AWS action or live opt-in was performed.

## PR CI and dependency closeout

The subsequent [CI sharding closeout](M7_CI_SHARDING.md) records the package-sized
hosted-runner timeout, exhaustive real-engine partitioning and fresh green CI at
`a5e1736`. M7 now incorporates the merged Go 1.26.9 dependency/security fixes;
the local full gate passed with zero reachable or imported-package vulnerabilities
(one unused-module advisory remains). This supplements, rather than rewrites,
the earlier Go 1.26.6 runtime acceptance checkpoint and its recorded counts.
It does not authorize deployment, opt-in activation or provider ambiguity clearance.

## Remaining boundaries

The activation deadline bounds how long a refusing, slow or offline member can
hold its peers (#209), and drain waits for a provider attempt only while it can
still run (#211). Corrupt-evidence blocks still keep their holds until an
explicit disable or setting the monitor back to `regional`, which drains them.

Backend implementation/PR acceptance is not live activation or release acceptance.
Ongoing vulnerability monitoring, the complete section-13 scenario matrix,
production-sized migration/lock/disk rehearsal, load/soak/canary/provider testing
and controlled release gates remain separate. Restrictive history retention/GC
and edge total-size accounting for facts/preparations remain follow-ons. No
confidential configuration, endpoint, payload, hash, credential or check output
is exposed through new diagnostics or HTTP Views.
