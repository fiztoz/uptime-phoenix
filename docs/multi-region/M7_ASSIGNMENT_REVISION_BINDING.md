# M7 immutable assignment-set revision binding

**Follow-on acceptance (2026-10-12):** [Colima real-engine evidence](M7_COLIMA_ACCEPTANCE.md)
executed all required SQLite/MariaDB markers with no MariaDB skips. It supersedes
the earlier unexecuted-engine qualification below; earlier command results remain
historical. The foundation was committed as `d86e6f8`. Subsequent
[live session verification](M7_CONFIG_SESSION_BINDING.md) exercises immutable
bindings through real TLS/WebSocket transfer, durable ACK refusal, cold reopen,
ABA/reconnect and duplicate ACKs on both engines. Mode application and
release/rollout acceptance are still separate.

Follow-on to the [cutover evidence contract](M7_CUTOVER_EVIDENCE.md). This slice
captures the missing assignment-set revision in the exact protected configuration
at **build time**. It does not produce runtime-mode application receipts or apply
aggregate/both. Runtime paging stays regional.

## Frozen implementation contract

- `ProbeConfigAssignment.AssignmentRevision int64` carries the positive stored
  `monitor_probe_assignment_sets.revision`, selected alongside desired delivery
  mode in the existing source transaction. Missing/invalid set rows fail source
  reads; there is no binding synthesized at receipt/read time.
- `ConfigAssignment.assignment_revision` is optional on schema-v1 documents. A
  present value must be a canonical **positive signed-int64 decimal string**.
  Null, JSON numbers, zero, signs, leading zeros and overflow reject. Unknown
  unrelated optional assignment fields retain their historical ignored behavior.
- Absence is legacy **unbound**, represented internally as zero, never current
  revision or revision one. Synthetic/legacy definitions with zero omit the field
  when encoded; negatives reject. Fresh production source reads populate a
  positive value, and new snapshot hashes cover it for all desired modes.
- New decoders retain the exact value in `EdgeResolvedAssignment` and nonsecret
  `AcknowledgedAssignmentDelivery` facts for local and remote retained documents.
  Existing HTTP GET/PUT Views and query behavior remain unchanged. No field is
  exposed merely because a domain struct acquired one.
- The pure cutover evaluator requires the acknowledged assignment's binding to
  equal the current desired set revision, independently of probe-document
  revision. Legacy/unbound and stale/ABA document bindings stay pending even if
  an independently supplied hypothetical application attestation claims current
  identity. No attestation producer is added.

## Production compatibility and retained documents

The new field changes **newly built** documents. Existing retained plaintext and
ciphertext are never rewritten, enriched or backfilled. Exact receipt-selected
reads preserve their original binding (including zero). Old decoders ignore this
optional assignment field, and acceptance does not require a new capability.
`paging.aggregate.v1` remains informational desired-field support only.

The local execution reader currently re-encodes mutable source rows and verifies
the active exact hash. Preserve legacy operation without inventing a binding:
first encode the current definition normally; only if it fails the hash comparison,
try the same graph with *all assignment revisions set to zero*. Accept that legacy
form **only** when its complete encoded bytes hash to the stored active hash, and
return the zero/unbound definition. Both failed comparisons remain conflict.
No graph, credential, generation or source-freshness check is weakened. A bound
snapshot with a stale binding cannot pass the legacy comparison. Do not perform
this compatibility retry for new activation: a freshly built bound definition is
required there, so a stale prepared document cannot acquire a newer binding.

The ordinary local refresher only fast-returns an executable active definition
when **all** its assignments have positive bindings. A valid legacy execution
read remains usable but causes preparation/activation of a new bound document.
Otherwise the compatibility read would permanently prevent an unchanged legacy
graph from upgrading. The zero-assignment graph needs no per-member binding.

Binding changes participate in normal snapshot change detection even when desired
mode/generation/settings return to their previous values. Snapshot and assignment
revision counters are independent. Source edits, policy-only edits, binding-only
edits, removal/re-add and aggregate → regional → aggregate must not relabel an old
snapshot. Cold local/remote reads and lost/retried config ACKs retain exact original
content; a config ACK remains document acknowledgement only.

## Parallel ownership

- Main: shared domain fields, nonsecret repository projections, pure cutover
  checks/tests, the local refresh upgrade fast-path, integration review,
  documentation and all verification.
- Repository worker: `probe_config_source.go`, `probe_activation.go`, and new
  `probe_assignment_revision_binding_test.go` only. Real source/publication/local
  activation/legacy execution/retained read tests; both engine leaves authored.
- Wire worker: `config_snapshot.go`, `config_encoder.go`, `edge_config.go`,
  `local_delivery_config.go`, and new `config_assignment_revision_test.go` only.
  Optional strict decimal decoding, encoder compatibility, local/remote modes,
  large counters and retention into decoded/projection facts.

The repository worker was cancelled by the delegation timeout without delivering
its changes/tests. Main implemented and verified that ownership after the worker
stopped. The wire worker delivered its bounded code and tests; main integrated
and executed them. A later read-only cold-review invocation failed to start due
to an unrelated Pi extension load error; no independent cold-review result is
claimed and no harness/credential changes were made.

All paths above are under the existing `internal/` domain/service/adapter folders.
No migrations, new interfaces, dependencies, API selectors, authority writes or
provider I/O are added. Database schema remains migration 076.

## Verification

Evidence is under ignored `tmp/m7-assignment-binding-verification/`. Commands
use cached Go 1.26.6 with `GOENV=off`, `GOTOOLCHAIN=local`, `GOPROXY=off`,
`GOSUMDB=off` and no inherited database/provider credentials. Downloading modules
or a toolchain is disabled; test I/O uses controlled fixtures/local servers.

```sh
go test -race -count=1 -json -timeout 300s \
  ./internal/core/services ./internal/adapters/probe ./internal/adapters/repository \
  -run 'AssignmentRevision|TestDeliveryCutover|TestLocalProbeConfigRefreshService'
go list ./...
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
go test -race -count=1 -json -timeout 2400s -p 4 ./...
go test -race -count=1 -json -timeout 300s ./internal/adapters/probe
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
gofmt -l internal/
git diff --check
```

- Focused race (`focused-03.jsonl`): **240 named passes, zero failures, five
  MariaDB skips**. It covers strict optional decimal decoding/encoding, exact
  local/remote projections, independent revision counters, same-mode ABA hashes,
  policy-only stale preparation refusal, retained reads, exact legacy execution
  fallback and automatic bound-document upgrade. The source-corruption fixture
  temporarily disables CHECK evaluation only on its disposable single session,
  restores it before reading, and proves the reader independently rejects zero.
- Full backend race (`backend-race.jsonl`): **22 packages, 4,754 named passes,
  zero failures**, **348 named test skips and 13 package skips**. These counts
  include parent/subtest passes, not unique test functions. All five new required
  repository SQLite leaves executed; no MariaDB acceptance is implied.
- After the broad run, additional encrypted edge cold-load cases retain binding
  `9007199254740993` through real enrollment, accepted connection generation,
  protected SQLite configuration persistence, process-store reopen and decoder
  revalidation. HTTP cases also traverse recording, incident commits, final
  outbox authorization and controlled DOWN/recovery provider sends for
  legacy/regional/aggregate/both desired modes. Docker cases retain resource
  semantics and unsupported-resource refusals. The final **entire probe package**
  rerun (`probe-final.jsonl`) passed **1,477 named tests**, including **11 new
  lifecycle passes**, with no failures/skips. Production code did not change
  after the broad run; this test extension was checked by the package rerun.
- CGO-free build, vet, final lint (**0 issues**), formatting and diff checks passed.
  `go list ./...` found no temporary packages; private overlay copies live only
  in `.mutations`. The owned backend shell exited normally.
- Four private negative controls omit source capture, bypass the document binding
  comparison, prevent legacy refresh upgrade, or drop remote projection fidelity.
  Each failed its intended assertion; all unchanged controls passed
  (`mutation-result.json`). No production file was overwritten.
- Strict engine-parser regressions: **14 tests passed**. The gate now requires
  all five exact `TestAssignmentRevisionBindingContract/mariadb/*` leaves in
  addition to the original 12 remote-reader leaves, nine local-reader leaves and
  both HTTP API markers. Missing/skipped/failed leaves cannot be replaced by
  SQLite or parent passes.
- Earlier `focused-01` failed to compile three new test calls against narrower
  interfaces; no repository test executed. `focused-02` exposed test-fixture
  setup errors (receipt `created_at` and schema-enforced positive revision).
  Correcting the actual contracts/schema produced the final passing run; neither
  failed attempt is counted as a gate pass.

Existing local acknowledged-document and pure-cutover slices, plus this follow-on,
remain **uncommitted** on `feat/m7-receipt-backed-config`, HEAD `284bc35`. Real
MariaDB acceptance is **unexecuted**, including the new session/corruption leaf,
InnoDB reads, activation and publication behavior. The previous full release-gate
failure at baseline Go advisories remains open; `make gate-full` was not rerun.
No toolchain upgrade, migration, new dependency, runtime mode application, send
activation or regional suppression occurred. Nothing was pushed or deployed.
