# M7 receipt-backed configuration reads

This bounded follow-on to [protocol delivery compatibility](M7_PROTOCOL_DELIVERY.md)
retains the desired availability delivery mode through runtime decoding and cold
loads, and adds an internal read of the exact acknowledged remote configuration.
Base: `b6b4027` (the merged decision, desired-persistence and wire slices).

**This is not an applied-delivery-mode receipt or an activation barrier.**
Regional availability delivery remains active for every desired mode. No aggregate
paging caller, source suppression, UI selector, HTTP response change, new table,
or migration is introduced. The M7 implementation-plan checkboxes remain open.

## Contract

- `EdgeResolvedAssignment.DesiredAlertDelivery` comes from that exact document's
  optional `alert_delivery`. Absence resolves to `regional`; invalid wire values
  still reject the document. The name deliberately distinguishes desired mode
  from runtime application or send authority.
- `RemoteProbeConfigSyncRepository.ReadAppliedRemote(ctx, target)` reads the
  active configuration pointer, its exact durable receipt and its exact retained
  protected snapshot. It never selects the latest prepared revision or rebuilds
  from mutable assignment, monitor or notification rows.
- Target, revision and hash must agree. The receipt's count and first committed
  application timestamp must agree with the pointer. A missing active pointer
  returns `ErrNotFound`; an inconsistent tuple returns `ErrConflict`. Neither
  case falls back to another document.
- Reads capture one database snapshot. MariaDB explicitly requests a read-only
  REPEATABLE READ transaction instead of inheriting a session's READ COMMITTED
  default. SQLite uses its normal read transaction. The transaction ends before
  decryption and extension validation.
- Authenticated decryption binds stored metadata. The installed hub decoder
  validates the original bytes and recomputes their hash; its complete metadata
  and assignment count must match the receipt-selected snapshot. Wrong keys,
  damaged ciphertext, invalid decoded output and validator failures fail closed.
  Plaintext is cleared after decoding and error details remain redacted.
- The returned graph is confidential internal data, not an HTTP View. It is a
  point-in-time configuration read: it grants no current lease, provider-send,
  suppression or applied-mode authority. Revocation and later configuration
  changes still need operation-specific checks at the eventual write/send boundary.

The reader is groundwork for the later activation barrier; no production paging
caller uses it yet. A configuration receipt remains a document receipt even when
its acknowledged document contains `aggregate` or `both`.

## Implementation

| File | Role |
| --- | --- |
| `internal/core/domain/edge_runtime.go` | Explicit desired-mode field on decoded assignments |
| `internal/adapters/probe/edge_config.go` | Preserve the validated wire mode in edge and hub decoding |
| `internal/core/ports/probe_config_sync.go` | Receipt-backed read contract on the existing sync port |
| `internal/adapters/repository/probe_config_applied_read.go` | Exact receipt/pointer/snapshot selection, authentication and validation |
| `internal/adapters/repository/probe_config_applied_read_test.go` | Dual-engine selection, corruption, restart, cancellation and coordinated receipt-write cases |
| `internal/adapters/probe/edge_config_test.go`, `edge_config_delivery_test.go` | Actual encrypted SQLite cold loads and regional DOWN/recovery recording, outbox authorization and provider invocation for all desired modes |
| `scripts/m6_dual_engine_gate.py`, `test_m6_dual_engine_gate.py` | Require all twelve exact MariaDB case names; reject absent/failed/skipped leaves even if SQLite or parent cases pass |

Existing migrations `047` and `049` already retain protected documents and
configuration receipts. This slice changes no storage shape; there is no new
migration or dependency.

## Verification evidence

Commands executed with the cached Go 1.26.6 toolchain, downloads disabled and no
inherited database/provider credentials:

```bash
go test -race -count=1 -json ./internal/adapters/repository ./internal/adapters/probe \
  -run 'TestRemoteAppliedConfigContract|TestEdgeConfigActivationRetainsExactBytesAndColdValidation'
python3 -B -m unittest discover -s scripts -p test_m6_dual_engine_gate.py -v
golangci-lint run --timeout 3m
make gate-full GOTOOLCHAIN=local
python3 -B scripts/m6_dual_engine_gate.py --mariadb-image mariadb:11
# Separately, from a temporary tracked-HEAD baseline copy:
govulncheck ./...
```

- Focused race gate: **26 named passes, zero failures, twelve MariaDB skips**.
  All twelve new repository cases executed on SQLite. The HTTP and Docker cold
  load matrix executed legacy, regional, aggregate and both desired values.
  The four HTTP cases continued through real SQLite recording and final outbox
  authorization to controlled provider calls, asserting one regional DOWN and
  one recovery with the same source incident identity and persisted resolution.
  Checker results and the external provider are test doubles; this is not a live
  target/provider or separate-process acceptance campaign.
- Strict-gate parser: **14 tests passed**. Its new case exercises missing, skipped
  and failed M7 leaves, with parent and SQLite passes as negative controls.
- Go lint: **zero issues**. Formatting and the production core boundary check
  found no framework/driver/adapter imports in core.
- Two private Go overlays deliberately dropped the desired mode and disabled
  receipt-hash equality. The aggregate/both cold-load assertions and the
  mismatched-receipt test failed at their intended assertions; the unmodified
  controls passed in the focused gate. No worktree source was changed for these
  experiments.
- Actual MariaDB gate: **blocked before database creation**, exit 1 at the cached
  Docker-image preflight. No disposable DSN was configured. MariaDB selection,
  timestamp and concurrent-read acceptance remain **unverified**; skips are not
  passes. The strict runner now requires all twelve named cases when a usable
  local Docker socket and cached supported image are supplied.
- Full local gate: **failed at the vulnerability scan** (make exit 2; scanner
  exit 3), not a clean full-gate pass. Earlier stages passed: Go build, vet,
  formatting and race tests across **22 test packages**; Go lint with zero
  issues; frontend check with zero errors/warnings, **324 unit tests**, build,
  lint and **27 Chromium E2E cases**; Helm lint/render/topology checks; strict
  backend-gate parser and release-tool regressions. The ordinary race run had
  no configured MariaDB engine; its package passes do not close that acceptance.
- `govulncheck` found **twelve reachable standard-library advisories** under
  Go 1.26.6. A separate scan of a tracked `b6b4027` baseline copy reproduced the
  same twelve advisory identifiers and scanner exit 3. No new reachable advisory
  identifier was introduced by this slice, but the full gate remains blocked.
  Toolchain remediation is separate from M7 and was not performed here. The first
  baseline scan attempt used GOFLAGS overlays; the scanner honored file deletion
  but did not honor all source replacements and failed package loading. That
  attempt is not vulnerability evidence; the subsequent baseline-copy scan is.

The coordinated receipt test commits a newer receipt through the production
writer between the first and subsequent reader SELECTs, then checks that the
first read returns the complete older document and the next read returns the
complete newer one. It does not independently falsify every isolation anomaly
or establish production-sized contention behavior.

Initial verification failures were in the newly authored tests: treating
`CanonicalAlertDelivery`'s boolean as an error, trying to construct rows forbidden
by existing CHECK/FK constraints, and omitting the telemetry encoder when extending
a configuration-only fixture into a recording lifecycle. These were corrected
without relaxing the schema or changing production recording/delivery behavior.

## Next boundary

Still required before any activation: explicit source capability/application
negotiation (not inferred from document ACKs), exact applied-mode evidence and a
cutover barrier; durable hub-owned aggregate incident/intents and database fencing;
final source suppression and queued-send authorization; policy-change and replay
lifecycle acceptance; and scoped API/UI controls with persist/reload/error/conflict
coverage. Certificate, capacity, escalation and group-alert ownership are unchanged.
