# M7 source cutover evidence decision contract

Follow-on to [local acknowledged-document evidence](M7_LOCAL_DELIVERY_CONFIG_EVIDENCE.md).
This bounded slice adds a pure, unwired decision contract. It does not implement
source capability negotiation, mode application, a committed cutover barrier,
aggregate incidents/intents or regional suppression.

## Why this seam

`config.applied` and the receipt-backed diagnostic establish exact configuration
**document** content. Today's `paging.aggregate.v1` means only that the decoder
understands desired mode. Neither is a runtime-mode application receipt. Adding
an applied-mode claim to today's wire protocol would assert behavior the runtime
does not implement; accepting an unused claim would not advance the barrier.

The pure `EvaluateDeliveryCutover` function instead specifies the evidence a
future barrier must assemble, following the unwired `EvaluateAggregatePaging`
precedent. No port is added without a storage/runtime consumer.

## Input and decision contract

The caller supplies one point-in-time, trusted current assignment set and source
evidence. The set revision covers membership, policy, bindings and desired-mode
changes; each member keeps its exact generation. The trusted installation and
accepted current stream are supplied separately from source claims. Both local
and remote members participate; missing evidence never shrinks the member set.
Paused/offline members are not silently excluded from a mode cutover.

For each member, a complete proposal requires:

1. Explicit support for **mode application**, distinct from optional-field
   decoding. Nothing currently advertises or populates this stronger fact.
2. An exact active pointer whose matching durable receipt and retained document
   were authenticated/validated by the caller. The pure evaluator cannot itself
   prove database provenance. Document facts include this monitor's generation
   and desired mode. The follow-on [revision-binding slice](M7_ASSIGNMENT_REVISION_BINDING.md)
   additionally requires the document's immutable assignment-set revision to
   match the current set. No settings or decrypted graph enter the evaluator.
3. A **separate** mode-application attestation for that monitor, bound to trusted
   hub/probe identity, the current assignment-set revision, exact generation,
   exact acknowledged config revision/hash, explicit selected mode, and accepted
   current source stream. A configuration receipt cannot be used as this object.
   The [edge regional application seam](M7_EDGE_REGIONAL_APPLICATION.md) and
   [protected local-hub seam](M7_LOCAL_REGIONAL_APPLICATION.md) now store
   source-local effective regional facts. The follow-on
   [authenticated regional reporting](M7_REGIONAL_APPLICATION_REPORTING.md)
   preserves those exact facts at the hub under session/receipt authority. Its
   regional-only capability does not promise aggregate/both application and never
   populates `ModeApplicationSupported`; no aggregate-mode producer exists.
   The subsequent [delivery preparation command](M7_DELIVERY_PREPARATION.md)
   records a pending request only; neither its capability nor successful command
   receipt can populate application support or this attestation. Its
   [protected local request counterpart](M7_LOCAL_DELIVERY_PREPARATION.md) similarly
   persists only a pending request, never aggregate application or source quiescence.

An assignment revision is **not** a probe-document revision. They are compared
only within their own identity spaces. Matching mode and a timestamp after the
policy boundary cannot replace the explicit set-revision binding: an
aggregate → regional → aggregate change must reject an older aggregate proof,
even if the generation/document/mode happen to match and the clock rolls back.
The revision-binding follow-on captures the set revision in new document
assignments; legacy documents remain unbound. A future activation request and
separate authenticated persisted mode-application receipt must establish the
same binding. A document binding or ACK still does not prove runtime application.

The top-level states are `regional`, `pending`, and `evidence_complete`.
`regional` means no consolidated-mode cutover is requested. Absent legacy desired
mode is regional; unknown modes fail validation. Aggregate and both need complete
evidence from **every** current member. Empty membership and historical snapshots
stay pending, never vacuously complete. Duplicate/foreign sources or malformed
assignment identity fail input validation. Output ordering is deterministic and
inputs are not mutated. Reasons disclose only bounded codes, not hashes or facts.
`assignment_mismatch` intentionally includes a document without an entry for this
monitor as well as a conflicting generation: neither document covers the current
member. Historical regional/legacy input remains `regional` (no cutover needed),
not a consolidated-mode pending proposal. `assignment_revision_mismatch` includes
legacy/unbound, older, and future document set revisions; a hypothetical current
attestation cannot repair any of them.

`evidence_complete` is deliberately **not** `active`, `ready`, `eligible`, a send
permit or a suppression permit. For `both`, regional availability must continue;
for `aggregate`, regional availability cannot stop until a separately committed
barrier and final authorization exist. Certificate, capacity, escalation and
watchdog behavior are outside this contract. The evaluator performs no I/O and
has no production caller.

## Required next lifecycle work

Design authenticated stronger capability negotiation and a real source mode
application lifecycle together. Persist the exact assignment/document/stream
binding; do not derive it from observational diagnostic reads or source clocks.
A future barrier must revalidate the current assignment revision, all accepted
source identities and stored owner/epoch/expiry transactionally before committing.
Enqueue and final queued-send paths must obey that committed state, and cutover
must address already-in-flight provider I/O and crash ambiguity. Pure evaluation
and queue priority establish none of those dependencies.

Current/replay ordering, cold starts, reconnects, stale owners, both directions of
mode changes and rollback require production lifecycle tests on SQLite **and**
MariaDB before runtime activation. No M7 activation checkbox is closed here.

## Verification

Evidence is under ignored `tmp/m7-cutover-verification/`. Commands use cached Go
1.26.6, `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`, `GOSUMDB=off`, and a clean
environment with no inherited database/provider credentials.

```sh
go test -race -count=1 -json -timeout 180s \
  ./internal/core/domain ./internal/core/services \
  ./internal/adapters/probe ./internal/adapters/repository \
  ./internal/adapters/http/handlers ./internal/bootstrap \
  -run 'TestDeliveryCutover|TestDeliveryModeApplicationShape|TestAggregatePaging|TestAcknowledgedLocalDeliveryConfigs|TestLocalDeliveryConfig|TestLocalDeliveryConfigDecoderDialectAndModes|TestRemoteAppliedConfigContract|TestDeliveryConfigEvidenceAPI|TestAcknowledgedDeliveryConfigs|TestDeliveryConfigHTTP'
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
go test -race -count=1 -json -timeout 2400s -p 4 ./...
go test -race -count=1 -json -timeout 600s ./internal/core/domain ./internal/core/services
gofmt -l internal/
git diff --check
```

- Final focused race: **422 named passes, zero failures, 23 MariaDB skips**
  (`focused-final.jsonl`). This includes **134 new cutover/attestation shape
  passes** and unchanged aggregate decision and local/remote diagnostic contracts.
  Counts include parent and subtest passes, not unique test functions.
- Full backend race: **22 packages, 4,635 named passes, zero failures**
  (`backend-race.jsonl`). **343 named tests and 13 packages skipped**; no MariaDB
  acceptance is implied. That run contained 133 new contract passes; after cold
  review added the historical-regional precedence regression, the final **whole
  domain/services** race rerun passed **1,383 named tests**, including all 134
  new contract passes, with one existing skip (`core-final.jsonl`). No production
  behavior changed after the broad run.
- Final CGO-free build, vet, lint (**0 issues**), formatting and diff checks passed.
  `go list ./...` contained no temporary packages. All overlay copies live in
  Go-ignored `.mutations` directories; no production file was overwritten.
- Four negative controls failed the intended assertions: omitting the set-revision
  comparison (including ABA/crossed revision spaces), treating config ACK as mode
  application, ignoring document-hash equality, and dropping missing members.
  Every unchanged control passed. These are pure contract sensitivity tests, not
  storage or source-runtime application proof.
- Cold read-only review traced every completeness/identity path and found no
  false-completeness defect. Its diagnostic-granularity question is explicitly
  documented above, and historical regional precedence is now tested. Inputs,
  nested pointer facts and member/source slices remain unchanged; output sorts
  deterministically and exposes no hash/stream/time or send/suppression flags.

The local diagnostic and this slice remain **uncommitted** on branch
`feat/m7-receipt-backed-config`, HEAD `284bc35`; nothing was pushed or deployed.
Prior local/remote diagnostic MariaDB acceptance and the full release-gate/Go
advisory blocker remain open; `make gate-full` was not rerun for this backend-only
slice. The new evaluator has no database path requiring engine acceptance of its
own, no production caller, and no implemented mode-application producer. No
migrations, dependencies, frontend controls or authority writes are added.
