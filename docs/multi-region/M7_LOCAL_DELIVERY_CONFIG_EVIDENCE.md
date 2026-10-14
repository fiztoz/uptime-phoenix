# M7 local acknowledged delivery-configuration evidence

**Follow-on acceptance (2026-10-12):** [Colima real-engine evidence](M7_COLIMA_ACCEPTANCE.md)
executed all required SQLite/MariaDB markers with no MariaDB skips. It supersedes
the earlier unexecuted-engine qualification below; earlier command results remain
historical. Mode application and release/rollout acceptance are still separate.

Follow-on to the [opt-in diagnostic](M7_DELIVERY_CONFIG_EVIDENCE.md), committed as
`284bc35`. This slice completes local document evidence without changing runtime
paging behavior, mode selection, authority writes, storage schema or dependencies.

## Frozen contract

The existing `GET /api/monitors/:id/probes?include_delivery_config=true` response
and query contract stay unchanged. When a local evidence reader is wired, local
assignments use the same `delivery_config_evidence` states and exact decimal-string
revision/generation/mode fields as remote assignments. `local_unavailable` remains
the honest fallback for a reader that was not wired; it is never fabricated ACK.
Default GET/false GET and PUT do not read configuration documents.

The local reader selects one matching active pointer, durable receipt and exact
retained protected snapshot in the existing read-only repeatable-read protocol.
It authenticates/decrypts after releasing the transaction, validates the existing
local snapshot dialect and installed extension semantics, checks immutable
metadata/assignment count, and returns only nonsecret assignment facts. Plaintext
is cleared. A source edit or a newer prepared document does not replace the exact
acknowledged bytes. Missing or mismatched receipt/snapshot never falls back.

`LocalAppliedConfigReader.ReadAppliedLocal` is deliberately **not** reused for
operator diagnostics. It serves execution: reconstructing current mutable source
rows, requiring them to re-encode to the selected hash, and taking SQLite write
locks. It does not establish this diagnostic's durable receipt/authentication
contract. That production lifecycle stays unchanged.

The common stored-tuple SELECT protocol is extracted without widening the remote
API's UUID-only target boundary or changing its error semantics. Local decode uses
`ConfigInspector`, `DecodeLocalConfigSnapshot` and `LocalConfigValidator`; no new
wire dialect or runtime-authority claim is introduced. Scope, assignment-generation
matching and revision/grant revalidation remain in the existing core service.
Missing/mismatched stored evidence is per-member `unreported`/`unverified`.
Authentication or semantic-decoder failure refuses the **whole** opt-in response
with a redacted 503, preserving the established remote contract. This includes a
future binary whose registries cannot validate a retained dependency; a current
validator's rejection never silently substitutes newer settings or partial facts.

Document mode is still **desired content**, not applied delivery-mode evidence,
lease ownership, send/suppression authority or an activation barrier. Local and
remote regional DOWN/recovery paging remain active for all desired modes.

## Acceptance and verification

Executed local evidence is under ignored `tmp/m7-local-evidence-verification/`.
Commands use cached Go 1.26.6 with `GOENV=off`, `GOTOOLCHAIN=local`,
`GOPROXY=off`, `GOSUMDB=off` and no inherited database/provider credentials.

```sh
go test -race -count=1 -json -timeout 180s \
  ./internal/core/services ./internal/adapters/probe \
  ./internal/adapters/repository ./internal/adapters/http/handlers ./internal/bootstrap \
  -run 'TestAcknowledgedLocalDeliveryConfigs|TestLocalDeliveryConfig|TestLocalDeliveryConfigDecoderDialectAndModes|TestRemoteAppliedConfigContract|TestDeliveryConfigEvidenceAPI|TestAcknowledgedDeliveryConfigs|TestDeliveryConfigHTTP'
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
```

- Focused race: **80 named passes, zero failures, 23 MariaDB skips**
  (`focused-03.jsonl`). Counts include parent and child test passes.
- Strict gate parser: **14 tests passed**. The nine exact new local repository
  leaves and the local API leaf are required MariaDB markers; missing/skipped/
  failed leaves cannot be replaced by parent or SQLite passes.
- SQLite exercised real preparation and `ActivateLocal`, exact retained selection
  after desired/source edits and newer preparation, cold readers, receipt/hash/
  count/time/target mismatches, AEAD authentication, malformed decoder outputs,
  plaintext clearing on success and failure, cancellation and coordinated local
  activation between SELECTs. The first read returns the complete older tuple;
  the next returns the newer tuple. Existing remote contracts passed unchanged.
- The real scoped JWT/router API exercised local and mixed local/remote evidence,
  no reads on default/false GETs, real CAS changes returning 409, revoked grants,
  identical hidden/missing 404s, and desired edits that cannot overwrite older
  document facts. A real decoder with an unavailable checker registry refused
  the entire response with redacted 503, pinning the reviewed semantic-failure
  classification rather than changing remote error granularity.
- Decoder tests cover local push, direct Docker and opaque ACK preferences,
  legacy/regional/aggregate/both modes, metadata mismatch, cancellation and
  nonsecret projection containment.
- Test-only overlays removing receipt-hash equality and using the remote decoder
  for local documents failed the intended assertions; unchanged controls passed.
  No production file was overwritten. Cold review confirmed tuple/lifecycle/
  dialect/scoping parity and raised the now-explicit semantic-failure granularity
  decision; it did not establish MariaDB acceptance.

The first broad build/lint attempt discovered archived `.go` overlay copies in
ordinary Git-ignored `tmp/.../mutations` directories. Go's `./...` discovery ignores
neither Git ignores nor ordinary directories: those copies formed invalid extra
packages. Source-only build passed, and moving the task-owned copies into
`.mutations` directories made broad build pass and removed all temporary packages
from `go list ./...`. Logs were preserved, archived scripts/overlay paths corrected,
and the identified backend process tree stopped and verified exited. The abandoned
attempt is **not a pass**. The clean rerun completed:

```sh
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
go test -race -count=1 -json -timeout 2400s -p 4 ./...
gofmt -l internal/
git diff --check
```

- Full race: **22 test packages passed, 4,502 named passes, zero failures**
  (`backend-race-02.jsonl`). There were **356 skip events**: 343 named test skips
  and 13 package skips. These do not establish MariaDB acceptance.
- CGO-free build and vet passed. Lint initially caught one constant argument in a
  new test helper; simplifying the helper to its actual fixed operation resolved
  it. Final lint reported **0 issues** (`lint-final.log`). The affected local/
  mixed repository/API focused rerun passed again (**16 named passes, 11 MariaDB
  skips**, `repository-final-focused.jsonl`); production behavior was unchanged.
- Final broad build, vet, formatting and `git diff --check` passed after the
  helper cleanup. The owned backend shell exited. No new framework/driver/adapter
  imports entered core.

A read-only preflight found no local Docker socket at `/var/run/docker.sock`; no
image was pulled, no context switched and no database created. MariaDB remains
**unexecuted**, including the concurrent InnoDB read protocol. SQLite or package
success does not establish it. The previously recorded Go 1.26.6 release-gate
advisories remain a separate blocker; whole `make gate-full` is not rerun for this
slice. No frontend/manifests, migrations, dependencies or runtime write paths
changed. No runtime activation is authorized.
