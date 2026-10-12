# M7 acknowledged delivery-configuration evidence

**Follow-on acceptance (2026-10-12):** [Colima real-engine evidence](M7_COLIMA_ACCEPTANCE.md)
executed all required SQLite/MariaDB markers with no MariaDB skips. It supersedes
the earlier unexecuted-engine qualification below; earlier command results remain
historical. Mode application and release/rollout acceptance are still separate.

Follow-on to [receipt-backed reads](M7_RECEIPT_BACKED_CONFIG.md), base `db89b6c`.
Committed as `284bc35`. The subsequent [local document evidence](M7_LOCAL_DELIVERY_CONFIG_EVIDENCE.md)
reuses this View/state contract for wired local readers; this record retains its
historical remote-only scope and verification limits.
This is an opt-in, scoped configuration diagnostic. It does not apply a delivery
mode, advertise new capabilities, suppress regional alerts, create aggregate
incidents/intents, or enable a selector.

## Frozen wire and service contract

`GET /api/monitors/:id/probes?include_delivery_config=true` retains the existing
assignment response and adds `delivery_config_evidence` to each assignment.
Omission or `false` leaves the response shape and configuration-read work unchanged.
The query accepts one value, exactly `true` or `false`; empty, duplicate or other
values return 400. PUT behavior is unchanged.

The optional object has exactly:

```json
{
  "state": "acknowledged",
  "config_revision": "9007199254740993",
  "generation": "2",
  "alert_delivery": "regional"
}
```

`alert_delivery` is the desired mode in the exact acknowledged document, **not**
its applied runtime mode. The object carries its own revision/generation so it
cannot be mistaken for independently read fleet counters. Both integers remain
JSON decimal strings. Hashes, installation identity, checker/provider settings,
credentials and confidential documents never enter this View.

Other states have null revision/generation/mode fields:

| State | Meaning |
| --- | --- |
| `unreported` | No active remote configuration pointer/receipt has been reported |
| `unverified` | Stored pointer/receipt/snapshot evidence cannot be matched |
| `assignment_missing` | The acknowledged document does not contain this monitor |
| `generation_mismatch` | It contains the monitor under a different assignment generation |
| `local_unavailable` | This bounded reader does not supply local configuration evidence |

Authentication and `AccessService` visibility are checked before configuration
reads; a hidden monitor remains 404. Configuration/decoder errors or unavailable
wiring return a redacted 503 rather than a fabricated acknowledgement. Assignment
CAS revision changes during the diagnostic read return 409 `assignment_changed`,
with no mixed response; authorization is rechecked before returning evidence.
These are observational read checks, not database write/send fences.

The repository projects the existing authenticated remote configuration reader
into a dedicated nonsecret port. Core read services never receive configuration
payloads, secrets, endpoints or hashes. Reads are opt-in because authenticating a
complete retained probe document can be expensive; no cache or new dependency is
introduced. No storage shape or SQL write changes; no migration is needed.

## Acceptance and verification

Local evidence is under ignored `tmp/m7-delivery-evidence-verification/`. Commands
use cached Go 1.26.6, `GOTOOLCHAIN=local`, `GOENV=off`, `GOPROXY=off`,
`GOSUMDB=off` and no inherited database/provider environment credentials.

Executed focused command:

```sh
go test -race -count=1 -json \
  ./internal/core/services ./internal/adapters/http/handlers \
  ./internal/adapters/repository ./internal/bootstrap \
  -run 'TestAcknowledgedDeliveryConfigs|TestDeliveryConfigHTTP|TestMonitorRegionalHTTPFixtures|TestRemoteAppliedConfigContract|TestDeliveryConfigEvidenceAPI'
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
```

- Focused race: **55 named passes, zero failures, 13 MariaDB skips**
  (`focused-05.jsonl`). Counts include parent and subtest passes.
- Strict dual-engine parser: **14 tests passed**, including missing/skipped/failed
  negative controls for the new exact `TestDeliveryConfigEvidenceAPI/mariadb`
  marker. Parent and SQLite passes cannot substitute for it.
- SQLite real-path integration uses encrypted retained documents, a fresh reader
  with bootstrap's hub decoder, `AccessService`, JWT middleware, the real router
  and explicit HTTP Views. It covers preparation without ACK, exact older mode
  despite newer desired/prepared documents, ACK without activation, a committed
  CAS edit between document read and response (409), mixed local/remote evidence,
  retired/re-added generation, tuple mismatch, authentication failure, grant
  revocation and identical hidden/missing 404s.
- Unit/wire contracts cover null unproven facts, strict/duplicate/encoded query
  keys, ignored malformed unrelated parameters, no expensive reads on default or
  false GETs, scoped denial before configuration reads, cancellation, unavailable
  wiring, redaction and decimal strings beyond JavaScript's exact-integer range.
- Test-only Go overlays bypassing generation equality and substituting the
  current desired mode for acknowledged document content **failed the intended
  assertions**; unchanged controls passed. Production files were not overwritten.
- Cold read-only review found the whole-query validation compatibility regression;
  it was corrected to validate only the recognized opt-in parameter and pinned
  with unrelated malformed-query regressions. Follow-up review found no remaining
  correctness findings within the checked query/CAS/mixed-source scope.

The first broad `go test -race -count=1 -json ./...` attempt hit Go's default
10-minute package timeout during existing repository migration fixtures, before
reaching the new API case. It is **not** a pass. The corrected full backend run
uses the documented `-timeout 2400s -p 4` settings and **passed**:

```sh
CGO_ENABLED=0 go build ./...
go vet ./internal/...
golangci-lint run
go test -race -count=1 -json -timeout 2400s -p 4 ./...
gofmt -l internal/
git diff --check
```

Build, vet and lint exited 0; lint reported **0 issues**. Race completed with
**22 passing test packages, 4,474 named passes, zero failures and 346 skips**
(`backend-race-02.jsonl`, `backend-result.json`). Of those skips, **313 are named
MariaDB skips**: package success is not InnoDB acceptance. Parent/subtest passes
are both counted. Formatting is clean, no framework/driver/adapter import was
introduced into core, and the owned backend process exited.

MariaDB acceptance remains **unexecuted**: no disposable DSN is configured. The
previous Docker cached-image preflight block and the baseline Go 1.26.6 standard
library vulnerability gate remain open; see the preceding
[verification record](M7_RECEIPT_BACKED_CONFIG.md). The whole `make gate-full`
release gate has not been rerun for this follow-on. No frontend/manifests,
dependencies, toolchain pin, migrations or production write paths changed.
No activation is authorized.
