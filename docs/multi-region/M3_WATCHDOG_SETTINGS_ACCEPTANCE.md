# M3 saved watchdog settings and CLI verification

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Base commit: `5072e42`. This increment prepares saved source settings; it does
not enable either watchdog or complete M3. Codex owns source and verification.
Antigravity owns no files and performed only a pasted-source, no-tools audit.

## Accepted behavior

Migration 060 adds settings and notification membership on SQLite and MariaDB.
Registration-first serializable transactions fence complete settings replacement
by its independent revision. Unchanged intent is a no-op, including revision-zero
disabled defaults. Competing first edits produce exactly one winner. Late link
failure rolls back scalar changes and the revision. Disabled registrations remain
readable, but writes require enabled remote registration. Channel deletion removes
its membership by foreign key.

The saved source reader and dependency resolver retain watchdog-only channels and
templates even without monitor assignments. Disabled dependencies remain explicit.
Remote snapshots carry bounded exact `probe.name` / `probe.location` display
metadata from the registration. Unknown/case-alias properties cannot overwrite
those canonical fields. Local default snapshots stay disabled. The edge maps the
settings into resolved config but retains the enabled-watchdog guard. Enabling
saved intent cannot overwrite the last valid prepared snapshot in this build.

`phoenix-probe-admin watchdog` saves complete intent with an explicit enable choice
and metadata-only output. `status` reports its separate settings revision.
`register` accepts location. Values are range-checked before narrowing to int32.
Positive V1 loss intervals remain supported by the timing conversion: suspect is
45 seconds unless loss is 45 seconds or shorter, when suspect becomes half loss.
Reminders remain minutes. No new dependency, provider, monitor or permission exists.

## Verification evidence

The final evidence ledger and source hashes are recorded in
[M3_WATCHDOG_SETTINGS_EVIDENCE.json](M3_WATCHDOG_SETTINGS_EVIDENCE.json).
The focused dual-engine gate passed all 45 named test events (16 MariaDB-named
events), with no test failures or skips. This count includes parent test events;
it is not 45 independent scenarios.

The actual CLI process regression runs initial migrations and each subsequent
operation in a separate process against a private disposable SQLite DB. It proves
single-JSON stdout, persisted settings across restart, stale revision rejection,
required explicit enable selection, overflow rejection, complete disable and no
invented applied receipt. It passed with race detection. Real MariaDB storage is
covered separately by the repository contract, not by this CLI process fixture.

The final full Go race gate passed in 22 tested packages. It recorded
231 MariaDB-named pass events and zero MariaDB skips. The 13 package skips are
packages with no test files. Two unrelated named tests skipped: the opt-in real
MongoDB checker and the pre-existing Telegram test with a hardcoded URL. No test failed. CGO-free
build and zero-issue lint passed. All 22 Go/SQL source hashes match the final run.
No frontend or Helm files changed; their checks remain in whole-M3 acceptance.

```text
Commands executed: go test -race -count=1 -timeout=20m -json ./...;
  CGO_ENABLED=0 go build ./...; golangci-lint run; git diff --check
Engines and named tests exercised: SQLite and real MariaDB;
  TestProbeWatchdogSettingsContract/<engine>/SavedGraphWithoutMonitors,
  ConcurrentCASAndRestart, LateRollbackAndBounds, MigrationRoundTrip;
  TestProbeAdminWatchdogSettingsAcrossProcesses (SQLite command processes)
Passed / failed / skipped: 22 tested packages passed, 0 failures;
  231 MariaDB-named pass events, 0 MariaDB skips; 2 unrelated named-test skips;
  13 packages have no tests. Build passed; lint: 0 issues.
Acceptance criteria still unverified: operational watchdogs, commands/ACK,
  rotations/reset, bounded shutdown and actual fifteen-minute partition;
  final whole-M3 frontend/process gate.
```

## Retrospective

### Summary

A fresh-database operator command succeeded but emitted invalid JSON output.
Changing migration diagnostics from `fmt.Printf` to `slog.Info` preserves the
command's stdout result stream. The real process regression now passes.

### Root cause

`RunProbeAdmin` invokes `repository.RunMigrations` before encoding its explicit
result view. `RunMigrations` wrote `Applying migration: ...` to process stdout.
The caller's JSON decoder therefore failed before reaching the valid result.
Capturing only the command's injected output writer misses this independent
stdout write, and a DB with no pending migrations hides it entirely.

### Fix and validation

`internal/adapters/repository/migrate.go` routes progress through the logger.
`TestProbeAdminWatchdogSettingsAcrossProcesses` exercises the actual command entry
point in child processes and decodes the entire stdout as one JSON result. The
fresh-DB manual reproduction failed before this change; the regression passes
with it. The test retains the real migration and persistence paths.

The first dependency-graph fixture also failed correctly because Codex supplied
an unsupported `{{alert.message}}` variable. The renderer contract uses
`{{message}}`; correcting the fixture made the graph test pass on both engines.
No production template validator was weakened.

### Audit feedback and prevention

Antigravity claimed empty `bun.List` channels must generate invalid SQL. Actual
SQLite and MariaDB saves with empty membership succeeded. Its claimed revision-zero
creation defect also contradicted the intentional virtual-default/no-op contract.
Both claims were rejected with named test evidence, and Antigravity acknowledged
the corrections in conversation `b5e7f7af-0f46-4e0a-b9d5-39f805dc2767`.

Future reviews must inspect actual builder/dialect behavior before asserting SQL
output, and use returned revisions rather than assume every successful save
increments them. Independent lifecycle review also caught that the first Get
implementation unnecessarily rejected disabled registrations; the final store
permits these reads and retains write rejection, covered on both engines.

For runtime timing, the audit correctly identified that a delayed original health
receipt can be older than a timer tick. Its drain-before-tick proposal alone does
not prove ordering: a producer may timestamp before the drain and enqueue after
it. The integrator returned this counterexample, and the reviewer acknowledged
it. Resolve admission ordering or explicit late-sample semantics in the runtime
integration; do not silently shift receipt timestamps or drop degraded samples.

## Still unverified / next required work

Both watchdog loops, source timer/checkpoint coordination, generation-safe receipt
ordering, bounded durable progress before lease renewal, explicit probe provider
context, send reconciliation and mirror replay authorization remain required.
Keep enabled config guarded until they work through both composition roots.
Commands/offline ACK, rotations/reset, bounded shutdown and the actual 15-minute
partition acceptance also remain required by the full M3 contract. Do not mark
M3 complete based on saved settings, storage tests or an audit acknowledgement.
