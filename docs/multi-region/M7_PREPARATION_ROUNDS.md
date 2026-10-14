# M7 shared preparation request rounds (no mode activation)

Continuation after `7db24db`; **implemented and verified** for pending request rounds
only. This is not mode activation or release/operational acceptance.
The later [standby planner](M7_AGGREGATE_STANDBY.md) consumes current regional health,
not these requests as authority, and creates no deliverable work.

## Contract

Reuse exact `delivery.prepare` request IDs and immutable command results; no new
wire field/capability, source migration, desired-mode table, HTTP/UI or activation.
A hub round is monotonic per monitor (independent of assignment/document revision),
with a separately persisted random canonical UUID nonce. It is bound to installation,
positive current assignment-set revision and aggregate/both mode. The nonce prevents
identity reuse after explicit destructive downgrade/re-up or restored counters. Capture ALL current members and generations, including local/offline/paused
members. Membership is not a readiness/barrier claim.

New hub SQLite/MariaDB080 stores immutable rounds, members and exact per-source
requests. Latest round by integer identity is selected; changed set advances rather
than resurrecting an ABA's older round. One-day DB-clock lifetime; current failed
request (committed rejected/expired) may rearm after a fixed one-minute cooldown.
Clock rollback never promotes history or supplies a new expiry permit. New request
IDs bind full tuple plus integer round AND nonce; old commands/results and source
pending tuples stay immutable. No historical legacy request is relabeled as a round
request. Cooldown is minimum round age, bounding rearm to at most one new round per
minute, not an operator delay promised from the moment an error arrived.

Remote issuer retains independent committed report/document/current-set and stored
owner/generation/stream/DB-clock fences. It records protected command + round request
atomically. Existing source command identity already supports distinct exact requests;
receipts recover independently after expiry, never current eligibility. Transport
reconciliation starts only after a committed negotiated regional report, uses cached
facts as selectors only, independently checks stored authority each retry, and joins
on session cancellation. Old peers remain compatible; command preparation still
promises request persistence only, never aggregate application.

Local reconciliation is a separate use case/port, invoked by protected refresh.
It rechecks one selected committed configuration/receipt, current whole-source graph,
regional facts and desired set in its transaction. Existing local pending rows and
execution reads are unchanged. Reconciliation commits only a new round-bound request,
not another config activation or any provider/checker effect. Failure after config
activation is honest request-unavailability; configuration remains committed/regional
and next refresh retries. Same-receipt activation itself never backfills anything.

Round rearm checks only terminal requests for a currently selected exact document
and accepted stream, not old-document/retired-stream results. Round expiry is a
request-lifecycle boundary, NOT source quiescence or provider suppression. Late old
results may finish their own immutable journal; they never mutate the selected round.
A future barrier must independently match selected round + exact source request,
current set/document/stream and genuine applied mode. Every captured member remains
required; there is no empty/offline shortcut.

## Acceptance plan

Real SQLite/MariaDB current-round reuse, monotonic ABA, expiry, terminal cooldown,
late historical results, rollback, cold reopen, concurrency, cancellation and schema
constraints. Local and remote production entry points share round/member identity;
real encrypted edge/TLS command session verifies rearmed IDs and immutable old receipt
recovery with regional effective state unchanged. Strict exact MariaDB markers, full
backend race/build/vet/lint/parser/format/boundary checks and private mutations.
No release/load/soak/canary or aggregate activation acceptance.

Artifacts: `tmp/m7-preparation-rounds-verification/`.

## Final executed acceptance and cleanup

Commands executed with the sanitized toolchain above:

```sh
go test -race -count=1 -json -timeout 600s \
  ./internal/adapters/repository ./internal/adapters/probe ./internal/core/services \
  -run 'TestDeliveryPreparation|TestLocalDeliveryPreparation|TestPreparedProbeConfigContract'
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-preparation-rounds-verification/dual-engine.jsonl
# Internally: go test -race -count=1 -json -timeout 2400s -p 4 ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py
gofmt -l internal/ cmd/
git diff --check
go list ./...
```

Strict gate: **22 packages, 5,695 named race passes, 503 MariaDB passes**, exit0,
**1,564.0 seconds**. Zero failures, MariaDB skips, missing required markers,
malformed events or unrecognized skips. All **122 M7 markers**, including **15 new
exact round/local-refresh/live-session leaves**, run on InnoDB. The five recognized
optional/helper/platform skips are group-cycle/crash-child helpers, live MongoDB,
isolated disk-full and live Telegram. Counts include parents/subtests, not unique
functions; separate runs are never added. Full gate covers the final production,
tests and scripts; subsequent changes are comments/documentation only.

Task-owned container/anonymous volume removed by the gate; task-owned pinned image
removed only after proving zero users. All **three pre-existing containers preserved**,
zero extras; Docker config isolated, Colima settings unchanged, daemon left running.
No global prune, external database, AWS, credentials/decision configuration, frontend,
Helm, dependency/toolchain change, push/deploy or mode activation. Raw evidence:
summary.json, gate-exit.json, final-checks.json, mutation-results.json and
cleanup-result.json under the ignored artifact directory. Artifacts are not a
fresh-checkout prerequisite. make gate-full vulnerability remediation and full
section13/operational/load/soak/canary gates remain open; not rerun or claimed here.

## Remaining limits

No committed all-source barrier, stronger application capability, aggregate incidents,
send/suppression fence or in-flight/crash ambiguity handling. Request history and
source pending tables remain outside bounded edge metadata accounting; retention
policy is follow-on work. Required source readiness is not inferred from round/member
creation or successful command write. Existing release vulnerability/operational
gates remain open.

## Executed verification and review so far

Sanitized cached Go 1.26.6 with GOENV=off, GOTOOLCHAIN=local, GOPROXY=off and
GOSUMDB=off; no inherited database/provider credentials or Go downloads.

- focused-04: **45 named race passes**, zero failures, **21 MariaDB skips**;
  new round/domain/issuer/schema cases plus existing preparation/migration cases.
- focused-05: **150 named race passes**, zero failures, **46 MariaDB skips**;
  production local refresh and regional provider DOWN/recovery with corrupt request
  state, retained-request/cold/current failure/ABA/concurrency/cancellation/schema
  coverage, preparation compatibility and real transport negotiation.
- focused-06: **19 named race passes**, zero failures, **4 MariaDB skips**;
  corrected real TLS/WebSocket round rearm, immutable old command/result and source
  receipt recovery, both local refresh fast paths (success/refusal), negotiated
  periodic retry and unsupported-peer silence. Named counts include parent/subtest
  events; separate runs are never added. Skips are not InnoDB acceptance.
- CGO-free build, vet, lint (**0 issues**), format, diff and package discovery
  passed; no temporary Go packages or forbidden production-core imports.
- **14 parser regressions** passed. Strict disposable gate requires **122 M7
  markers**, including **15 new exact MariaDB leaves** (11 round, 2 local refresh,
  2 real source/hub session leaves). Parent or SQLite passes cannot replace them.
- Six private .mutations overlays disable expiry advance, cooldown, current-document
  filtering, full local membership, nonce identity, or request persistence. All
  fail intended assertions, never compilation; unchanged control passes.
  Production files are not overlaid. No engine acceptance claim for these controls.

### Failed-run ledger

focused-01/repro-01 deterministically fail the old collision fixture DELETE on
probe_commands because the new exact request FK correctly retains the journal.
No debugger was installed. Disproof overlay removes only its test-owned child
reference before collision replacement; repro-disproof passes. Fixture corrected,
not the production FK. focused-02 also exposed a mismatched local fixture protector
and a noncanonical stale owner: mixed-source publication now uses the real shared
protector/installation; owner test uses an independent canonical UUID. focused-03
showed that a selected owner constant was actually the current owner; corrected the
oracle with an explicit different-owner assertion, never the fence. Initial lint
unparam/whitespace findings corrected. session-01 failed the new test-only SELECT *
projection on updated_at; explicit selected columns match the actual test DTO.
focused-05/06 and final lint pass. Failed logs remain local for cross-reference.

### Smaller-reuse and production-path review

Goal: make expired/current-failed preparations retryable across ALL assigned sources
without rewriting historical receipts or inventing aggregate behavior. Smaller
reuse: command_id already gives source persistence/deduplication/exact recovery;
adding a wire round field/capability or rebuilding edge command schemas is unnecessary.
Keep the shared integer/nonce/member selection at the hub and derive new existing
command IDs. Separate local request reconciliation avoids turning preparation
history into a heartbeat or pre-send requirement.

Trace: committed negotiated report → independently fenced issuer → current set →
validated complete immutable round/member selection → protected command and exact
request reference in one transaction → real source command journal/pending tuple →
immutable fenced result. An independent joined transport worker retries ONLY captured
committed-report selectors; each retry repeats stored authority/prerequisite checks,
never trusting cache or queue ordering. Local protected refresh (both stable paths
and fresh activation returns) → independently selected committed graph/receipt →
honest regional prerequisite → same all-member round selection → local exact request.
Provider/checker I/O never enters these transactions. Failure after configuration
commit is request-unavailability; the configuration and effective regional facts
remain committed and next refresh retries.

Manual inspection found no correctness blocker in this bounded path; it is not a
separate independent review or engine execution claim. Extra full-graph/cryptographic
checks on periodic reconciliation have NOT been load/soak accepted. Late old source
requests may still persist results after the round is superseded: round expiry is
not remote command cancellation, a source stop proof or provider-I/O fence.

080 down explicitly discards pending-only round/request membership, not commands,
local079 records or source pending/receipts. Re-up creates no facts from old rows.
New nonce prevents integer re-use from aliasing retained requests. Snapshot/journal
FKs restrict deletion while requests remain retained; coordinated retention/GC is
still follow-on work, not a successful unbounded metadata-budget claim.
