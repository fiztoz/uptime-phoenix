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
   then issue protected `delivery.quiesce` commands. A source persists its hold
   before checking unresolved provider attempts. A busy refusal is not quiescence.
3. **QUIESCED:** advance only after all exact quiesce successes are committed.
4. **APPLYING:** issue `delivery.apply` against the exact quiesced gate and captured
   document. Persist source application and immutable kind-specific receipt.
5. **ACTIVE:** advance only after all exact applications commit. Genuine aggregate
   incidents, transition/recovery/repeat intents and delivery claims use separate
   operative tables and the existing pure paging/cutover evaluators.
6. **DRAINING:** disable or membership/document/policy/generation/stream drift stops
   admission and supersedes safe unstarted plans. Unresolved admitted hub provider
   work blocks resume. Only exact original-member `delivery.resume` successes
   release the source holds.
7. **RELEASED:** a subsequent deliberate opt-in needs a new barrier. Local
   application history includes barrier identity, so the same retained document
   can be applied in a later cycle without aliasing or rewriting the earlier fact.

Paused, disabled, offline, missing and removed-but-not-resumed sources do not
shrink the barrier membership. Expiry, reconnect, reset and configuration
replacement cannot erase a source hold. Resume-before-quiesce leaves an exact
released tombstone, preventing a delayed old quiesce from reinstalling a hold.
Blocked established barriers remain discoverable for explicit disable/drain.

Disable is not regional resume. Startup refuses removal of protection/runtime
needed by durable authority, and migration downgrade refuses unresolved authority.
No operator or automatic ambiguous-outcome override is implemented in this slice.
An ambiguous request can intentionally prevent drain; do not edit its journal to
invent success or manufacture a historical completion timestamp.

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

Backend implementation/PR acceptance is not live activation or release acceptance.
Ongoing vulnerability monitoring, the complete section-13 scenario matrix,
production-sized migration/lock/disk rehearsal, load/soak/canary/provider testing
and controlled release gates remain separate. Restrictive history retention/GC
and edge total-size accounting for facts/preparations remain follow-ons. No
confidential configuration, endpoint, payload, hash, credential or check output
is exposed through new diagnostics or HTTP Views.
