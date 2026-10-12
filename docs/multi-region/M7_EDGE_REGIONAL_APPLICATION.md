# M7 edge regional availability application state

Bounded follow-on to `324c08b` and [live configuration-session verification](M7_CONFIG_SESSION_BINDING.md).
Implemented and verified on `feat/m7-receipt-backed-config` (2026-10-12).
This records a bounded source-local production seam, not M7 activation.
The subsequent [protected local-hub counterpart](M7_LOCAL_REGIONAL_APPLICATION.md)
has separate activation/retry/verification semantics; the evidence below remains
specific to this edge slice.

## Scope and authority

The edge currently implements regional availability delivery only, independent of
`DesiredAlertDelivery`. Persist this honest effective mode for bound assignments
in the edge's own configuration activation transaction. Consume present state in
actual availability recording, escalation enqueue, and final send authorization
transactions. No aggregate implementation, suppression branch, provider sends on
the hub, local hub application producer, new wire frames/capabilities, HTTP Views,
UI controls, cutover caller or application-reporting port is added.

`ProbeDeliveryModeApplication` describes the exact internal tuple: trusted target,
monitor, immutable assignment-set revision, assignment generation, exact document
revision/hash, source stream, and effective mode. Only **regional** is accepted by
this producer. Desired aggregate/both never populates its effective mode. Zero
assignment binding produces no application fact; missing legacy state keeps the
currently implemented regional behavior, but supplies no cutover evidence.

These are source-local durable facts, not authenticated hub-observed application
reports. They do not set `ModeApplicationSupported`, complete a cutover, prove
cessation of regional sends, or grant send/lease authority. Existing fences remain
required. Certificate, capacity, watchdog, and hub-local delivery are unchanged.

## Storage and lifecycle

- Add private edge migration `015_regional_delivery_application.tx.up.sql` and
  down companion. Store only bounded nonsecret identity fields, no config graph,
  credentials or endpoints. Keep the existing protected configuration immutable.
- `EdgeConfigService.Apply` explicitly builds regional facts from the authenticated
  resolved document and the accepted local stream. `EdgeActiveConfig` carries
  those facts to `ActivateConfig`; no new port is needed.
- Validate exact config/target/hash/monitor/generation membership and positive
  assignment revision. Facts must be regional, unique and on the current stream.
- Commit facts with configuration/index selection. Failed/stale activation leaves
  neither new facts nor a new active pointer.
- A same-document retry may establish missing facts from that exact authenticated
  document, including after upgrade, without rewriting ciphertext or activation
  time. Existing facts must compare exactly; never repair corrupt rows or look up
  mutable desired revisions. No migration backfill claims application.
- Retain stream identity in the row key. A stream rollover does not rebind old
  facts to a new stream; current reads select only the accepted stream.
- Read present facts through `ReadActiveConfig`. Cold `Load` validates them against
  the exact authenticated retained document, not mutable source rows. Absence is
  allowed for legacy/pre-upgrade state and remains unreported.
- Availability recording/escalation/final authorization validate any present
  fact in their existing database transaction, checking selected document hash,
  target, generation, current stream and regional mode. Invalid present facts
  fail explicitly; absence never suppresses existing regional paging. This is not
  aggregate suppression or a replacement for existing ownership/lease checks.

## Required verification

Effect-based tests must cover exact positive binding above JavaScript precision,
regional/aggregate/both desired modes, legacy absence, activation rollback and
stale generation, same-document upgrade/retry immutability, cold reopen,
stream rollover isolation, corrupt present state, availability recording/final
send participation, and unaffected certificate/capacity/watchdog paths.
Supplement unit tests with real encrypted configuration and live-session tests.
Edge storage itself is SQLite; the changed hub source publication/receipt lifecycle
also requires real MariaDB execution.

## Executed evidence

Used cached Go 1.26.6 with `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`, and
`GOSUMDB=off`, sanitized environment, and no module/toolchain downloads. The strict
gate used the existing Colima daemon and the same official MariaDB image digest
as the preceding acceptance records, in a fresh disposable database.

```bash
go test -race -count=1 -json -timeout 300s \
  ./internal/core/services ./internal/adapters/repository/edge ./internal/adapters/probe \
  -run 'TestEdgeRegionalApplication|TestEdgeConfig|TestEdgeEscalation'
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11 \
  --go-json tmp/m7-edge-regional-verification/dual-engine.jsonl
# Gate executes: go test -race -count=1 -json -timeout 2400s -p 4 ./...
go test -race -count=1 -json -timeout 300s ./internal/adapters/repository/edge
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
gofmt -l internal/
git diff --check
```

| Executed evidence | Result |
|---|---:|
| Initial focused race | 99 named passes; no failures/skips |
| Full strict dual-engine race | 22 packages, 5,233 named passes |
| Named MariaDB passes / skips | 404 / 0 |
| Named failures / missing required markers | 0 / 0 |
| Required M7 MariaDB markers | all 31 passed |
| Recognized optional/helper skips | 5, same as preceding gate |
| Final entire edge package race | 260 named passes; 31 new application passes; no failures |
| Final edge skips | existing crash-child entry and disk-full platform fixture |
| CGO-free build / vet / lint | passed; final lint 0 issues |

Counts include parent/child events, not unique functions. The final edge run is
separate, not additive to the full-gate count. After the full gate, only comments
and a new reset-lifecycle test changed; the whole edge package and lint reran.
The three mandatory live-session MariaDB leaves now assert persisted effective
regional facts through real TLS transfer, refused hub receipt, cold reopen,
old-document retry after ABA, and new-document acknowledgement for every desired
mode. No source-local fact is fed into hub cutover evaluation.

The new storage tests exercise full rollback, exact positive bindings, immutable
same-document upgrade/retry, current-stream isolation, absent legacy behavior,
schema constraints, read/record/authorization corruption refusal, escalation
intent rollback, certificate-subject authorization and migration up/down/cascade.
The reset follow-up uses the real protected archive/reset/reopen entry points:
original-stream facts remain archived/retained, never selected for the new stream;
corrupt current facts explicitly refuse reset. Its fact is seeded in a storage
fixture; authentic document production is established by the live session tests.

A bounded independent read-only review found no correctness blocker. The suggested
removal/relabeling of the escalation currency check was not adopted: stale planned
intents must refuse an intervening configuration activation. Only no-intent ladder
cleanup bypasses this application check. Updated domain comments distinguish the
new source-local regional producer from absent authenticated hub reporting.

Five private overlays substituted desired mode for effective regional, omitted
fact persistence, or removed recording/final-authorization/escalation checks.
Each failed its intended effect assertion; all five unchanged controls passed.
Production source was never overwritten. Overlay copies are under `.mutations`
and no temporary Go packages were discovered. Artifacts:
`tmp/m7-edge-regional-verification/`.

## Cleanup and remaining limits

The gate removed its owned disposable container and anonymous volume. The exact
owned downloaded image was removed without force after checking its ID and lack
of users; no fixture containers or volumes remained, and the owned gate shell
exited. Existing stopped containers and Colima configuration were untouched.
Colima remains running; AWS was not used. No push, deployment or activation.

Valid certificate/capacity/watchdog behavior is unchanged. Certificate final-send
bypass is directly tested; existing capacity/watchdog regressions ran in the full
gate, but additional fact-bearing capacity/watchdog fault cases were not authored.
Corrupt present application state can refuse the whole authenticated config load
and source check transaction, affecting other work sharing that configuration;
it is not silently repaired or bypassed. A missing row remains unreported and
regional, so this table is deliberately not a future suppression fence.

Like the existing certificate/condition extension tables, these rows are not
charged to the five-table `edge_metadata_budget` counter. They are bounded by
retained configuration membership and cascade with document deletion; the counter
must not be represented as an absolute total SQLite-size limit. Extending metadata
accounting remains separate work before larger operational activation.

`make gate-full` was not rerun. Previously reproduced baseline toolchain advisories,
full failure/operational acceptance, local hub effective-mode state, authenticated
capability/application reporting, committed cutover barriers, aggregate incidents
and queued/in-flight suppression remain open. Regional sends remain active.
