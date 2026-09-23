# M4 remote certificate paging

This slice lets a remote HTTPS monitor page its certificate expiry. The source
that owns the assignment decides when a threshold is reached, sends through its
own durable outbox, and the hub mirrors the result. It advances M4's auxiliary
evidence requirement; capacity state, escalation, recovery, backup and
deployment compatibility remain open.

## Behavior and ownership

Certificate paging runs at the edge, never on the hub. `EvaluateCertAlertPaging`
is a pure evaluation over the accepted assignment graph, the durable cursor read
in the same transaction, and the maintenance verdict resolved from the windows
named by that assignment. It keeps the local algorithm's delivery rules: opt-in
through `cert_expiry_notify` only, an exact certificate `NotAfter` (rounded days
never identify a certificate), thresholds 30/14/7 firing the most urgent crossed
one once, and a renewed certificate resetting the delivered history.

The cursor lives in `edge_cert_alert_state`, keyed by monitor and assignment
generation like every other regional auxiliary store. It holds the most urgent
threshold already committed, the exact expiry that threshold was delivered for,
and the one open certificate incident representing that pair. Those three fields
are one invariant: an empty incident pointer always pairs with a zero threshold
and a nil expiry, so a renewed certificate can never inherit the previous
certificate's history. A `version` column fences every cursor write the same way
the state sequence fences retry state — a record built against a superseded
cursor returns `ErrStaleLocalState` and is re-evaluated rather than applied.

Incidents and the cursor are source-owned. The cursor never crosses the wire: the
hub sees `alert.transition` events carrying the immutable certificate subject
(threshold plus exact expiry) and `delivery.result` events with event kind
`certificate_expiry`, and mirrors them into `probe_incidents` and
`probe_delivery_events`. It never infers a delivered threshold from raw
evidence, and mirrored certificate history creates zero hub provider intents.

Crossing to a more urgent threshold retires the superseded incident first, then
opens a new identity, in that order, so a replay can never observe two open
incidents for one assignment. A partial unique index makes that a storage
invariant, not a convention. Retirement without a replacement — recovery past
30 days, or renewal while an incident stayed open — closes the incident
administratively and clears the threshold, and creates no provider work.

Maintenance suppresses the entire lifecycle, including the administrative
retirement, so a window can neither consume a threshold the operator was never
told about nor quietly close an incident that is still factually open. Absent or
malformed certificate evidence is never treated as recovery.

Each committed alert stores its rendered snapshot with the delivery intent:
threshold, whole days remaining, bounded issuer, exact expiry and message. A
retry after a restart therefore delivers the threshold it was raised for rather
than a value re-derived from a later clock or from a pruned observation. The
delivery authority revalidates that snapshot against the stored incident before
any provider call, so an advanced or retired incident supersedes pending work
instead of sending a stale alert. Checker metadata beyond the three public
certificate fields never reaches storage or a provider.

The remote acknowledgement command targets availability incidents only, so a
certificate incident cannot be acknowledged through this path; the hub refuses an
`acked` certificate transition, and `include_ack_url` remains rejected by remote
activation. That boundary is M5's administrative workflow, not an omission here.

## Operator limits and upgrade order

A monitor's `cert_expiry_notify` flag now reaches remote activation instead of
rejecting the whole snapshot. Certificate paging therefore requires the upgraded
edge, and an older edge still rejects a snapshot that asks for paging
explicitly rather than silently ignoring it — mixed-version rehearsal remains
open operational work.

Run schema changes with all application writers stopped. Edge migration `011`
rebuilds `edge_alerts`, `edge_delivery_outbox` and `edge_watchdog_state` in one
transaction to widen their subject and event-kind checks and add certificate
columns; `edge_delivery_outbox` is a child of `edge_alerts`, so the children are
rebuilt and dropped before the parent, and the metadata-budget triggers are
recreated against the rebuilt tables. The new cursor table deliberately carries no
budget trigger: its row count is bounded by live assignments and pruned with them,
and a trigger there would be orphaned by the 010 budget migration's down path.

Hub migration `067_probe_certificate_paging` adds `certificate_not_after` to
`probe_incidents`. X.509 validity instants are whole seconds, so this table's
microsecond timestamp form is lossless for the identity. `067`'s down path refuses
to drop the column while any mirrored certificate incident or `certificate_expiry`
outcome exists. Do not delete history to force a downgrade.

Certificate paging stores no evidence columns of its own; the certificate's
current view on the hub remains the `tls_info` projection written by the
[TLS evidence slice](M4_TLS_EVIDENCE.md).

## Verification coverage

Authored coverage is `TestCertAlertPaging*` (pure evaluator),
`TestEdgeCertificatePaging*` (real recorder, encoder and edge SQLite),
`TestCertificateIncident*` (wire encode/decode/replay mapping) and
`TestProbeCertificatePagingAcceptance` (both hub engines). Commands and executed
results are in [Testing guide](../TESTING.md) and recorded below.

## Test harness correction this slice required

Building the harness for a real MariaDB run exposed a pre-existing shared-schema
hazard that this slice newly trips. The registry migration rehearsal walks the
whole disposable database down to its own boundary and restores it afterward, and
MariaDB cannot roll DDL back, so a sibling test starting inside that window reads
`probe_incidents` through a model that names a column only the rehearsal had
removed. Because `_migrations` still records the migration as applied,
`RunMigrations` alone can never heal the drift. Two files observed 91 such
failures while the product code was correct.

The fix is in the harness, not the schema contract: `healMariaDBTail` re-applies
the idempotent tail migration whenever its post-condition is missing, so every
MariaDB fixture starts from the shape the model expects, and the rehearsal restore
loop now continues after a failure instead of `return`ing and stranding the shared
database at an older shape. `067`'s MariaDB scripts are written idempotently
(`ADD/DROP COLUMN IF EXISTS`) to make that healing possible, and its downgrade
guard names only columns that outlive the migration so it reports the guard rather
than an unrelated unknown-column error. `TestProbeCertificatePagingAcceptance` also
refuses to run the destructive `down` script at all unless the row that must trip
the guard is provably present.

The evaluator matrix asserts the threshold lattice, one-alert-per-threshold
suppression including the 30-days-covers-14/30 rule, ordered advance, renewal
opening a new identity, recovery without provider work, maintenance consuming
nothing, active/inactive channel fan-out, a named channel missing from the
accepted graph failing closed instead of being skipped, and that every suppressed
path allocates no identity at all.

The durable lifecycle drives the production recorder and encoder: incident row
identity, cursor pointing at exactly that incident, the observation-then-transition
event order, the immutable delivery snapshot, a same-threshold recheck paging
nothing before and after a process close/reopen, an advance emitting two ordered
transitions, a stale cursor fence returning `ErrStaleLocalState`, and a rejected
lifecycle leaving alerts, events, provider work and the stream sequence
byte-for-byte unchanged.

The end-to-end effect test serves a real self-signed certificate expiring in five
days over HTTPS, runs the production HTTP checker, the real store and the real
delivery worker, and asserts exactly one provider call with the correct rendered
fields and regional scope; that a transient provider failure retries the same
durable intent rather than paging twice; that the outcome becomes
`delivery.result` telemetry; and that further checks across two more restarts
never reopen a delivered threshold.

`TestProbeCertificatePagingAcceptance` runs on SQLite and MariaDB: mirrored
lifecycle with zero `probe_delivery_intents` and an un-inferred `tls_info` cursor,
immutable subject enforced against restated threshold and expiry, acknowledgement
refused, orphan delivery refused, duplicate receipt adding no rows, and the
populated 067 downgrade guard.

Executed results are recorded below and in the accompanying
[hashed evidence](M4_CERT_PAGING_EVIDENCE.json).

## Executed evidence

Commands executed:

- `CGO_ENABLED=0 GOTOOLCHAIN=go1.26.6 go build ./...` — pass.
- `GOTOOLCHAIN=go1.26.6 go test -json -race -count=1 -timeout=2400s -p 4 ./...`
  with the disposable `phoenix_ci` MariaDB DSN, including
  `parseTime=true&loc=UTC&multiStatements=true` — pass.
- `GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run --timeout=5m` —
  zero issues.
- `gofmt -l internal`, `git diff --check`, production core framework/driver-import
  inspection and changed-document link checks — clean.
- `python3 scripts/m4_cert_paging_evidence.py --gate <race json> --mariadb-version
  11.8.9 --baseline c400b9c --date 2026-09-23` — exit 0, which requires zero gate
  failures **and** every named MariaDB acceptance case to have executed; it writes
  [the manifest](M4_CERT_PAGING_EVIDENCE.json) with the hashes of all 45 changed
  source files.

Engines and named tests exercised:

- Edge SQLite, hub SQLite and MariaDB 11.8.9 on the local disposable container
  `phoenix-m04-checker-coverage`.
- Every evaluator, durable, wire and both-engine acceptance case named above
  executed. The focused command is recorded in
  [Testing guide](../TESTING.md).

Passed / failed / skipped:

- Full race gate: 3,851 named passes across 21 tested packages, **zero failures**,
  completed in 796 seconds.
- Two optional skips, both pre-existing and unrelated: `TestDatabaseChecker_Check_MongoDB_RealServer`
  and `TestTelegramSender_Send_DownSeverity`.
- 60 named passes carry this slice's certificate-paging markers, of which 6 are the
  MariaDB acceptance cases. The evidence generator requires all five named MariaDB
  leaf cases to have executed and fails otherwise; none skipped.
- The `internal/adapters/repository` package ran green as a whole (81.6 seconds)
  against real MariaDB after the harness correction above; before it, 91 tests in
  that package failed on shared-schema drift.
- No product-code failure was observed in any run: every drift failure was the
  `Unknown column 'inc.certificate_not_after'` signature described above.

Unverified acceptance criteria:

- No compiled-process gate exists for this slice. `scripts/probe_runtime_smoke.py`
  has no `--verify-cert-paging`; the existing `--verify-tls` fixture certificate is
  not inside the 30-day window, so real-process paging, restart and replay across a
  hub link have not been exercised end to end. The Go matrix above is the evidence.
- A successful 067 `down` is exercised on SQLite only. MariaDB cannot roll DDL back
  and the shared disposable CI schema is read by parallel packages, so the
  destructive direction would open a window in which unrelated `probe_incidents`
  readers fail; the 067 `up` path runs through `RunMigrations` in every MariaDB
  fixture and the guard is asserted on both engines.
- Capacity state and two-sample promotion on remote probes remain unimplemented, so
  the combined M4 auxiliary requirement stays open.
- Mixed-version deployment rehearsal, escalation, recovery policy, backup/restore
  and Helm/deployment compatibility, and M5 regional administrative/browser views.
- The previously accepted M3 partition, watchdog, ACK, rotation and stream-reset
  process scenarios were not rerun for this slice; their Go regression tests were
  included in the full suite.

This is local engineering acceptance. Nothing was pushed, deployed or migrated in
a production database.
