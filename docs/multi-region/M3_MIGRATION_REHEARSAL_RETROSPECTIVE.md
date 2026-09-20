# Migration rehearsal schema-loss retrospective

## Summary

The credential source gate exposed an older test-harness defect from the ACK
increment: the registry migration rehearsal rebuilt the shared MariaDB schema
through migration 060, omitting 061 while leaving the migration ledger at 061.
The prior full suite could pass and leave a database that failed the next ACK
suite. Codex replaced the hand-maintained restoration list with the complete
discovered migration suffix and added a before/after command-column assertion.

## Root cause

`testProbeRegistryMigration` in `probe_registry_test.go` manually downgraded the
dependencies of migration 035, rehearsed 035 and restored a fixed list. That list
ended at `060_probe_watchdog_settings`. Migration 036's downgrade drops
`probe_commands`, so restoring through 060 recreated only the original ten-column
table. Migration 061's eleven additional command columns disappeared.

The test executes SQL files directly and deliberately does not change the real
migration ledger. A later `RunMigrations` therefore saw 061 as already applied and
did not repair the table. SQLite fixtures used a fresh file for each test, hiding
the leftover-state problem. MariaDB reused the disposable database across runs.
ACK tests occurred before the registry rehearsal, so a fresh first full run could
pass before the rehearsal damaged the state used by the next run.

## Reproduction

The first source credential full gate failed the MariaDB ACK issuance cases with
`probe command storage failed`. Read-only schema inspection showed only the
original command columns and no fault-injection triggers. A new before/after
column assertion reproduced the same loss in an isolated SQLite rehearsal:
`hub_id`, `stream_id`, `payload_sha256`, `protected_payload`, retry fields, result
fields and `retain_until` were present before and absent after cleanup.

The initial assertion used `t.Context()` during `t.Cleanup` and failed with
`context canceled`, masking the schema comparison. It now uses a separate bounded
five-second cleanup context. The corrected assertion then exposed the actual
eleven-column loss before the restoration logic was changed.

## Fix

The rehearsal discovers every later `.up.sql` file in the selected engine's
migration directory, verifies its matching down migration, and downgrades in
reverse order. Cleanup is registered before the first downgrade and restores
successfully downgraded scripts in forward order. It compares the restored command
columns with the original schema. There is no hard-coded latest migration number.

This is a test-harness correction. Production migrations and the production ACK
implementation are unchanged. The failed disposable database is not evidence of
a production upgrade losing columns.

## Validation and lesson

The isolated SQLite regression failed before and passed after the fix. The ACK
storage/replay and registry migration contracts passed twice on SQLite and a
fresh disposable MariaDB schema: 60 MariaDB-named pass events, no failures and no
skips. The subsequent full source credential build/race/lint gate also passed. The repeated-run results
are recorded in [the rehearsal evidence](M3_MIGRATION_REHEARSAL_EVIDENCE.json).

Migration rehearsal success must include restored schema usability after cleanup.
A passing first test run is insufficient when its teardown mutates a shared engine.
Codex owns this missed integration dependency; the failure is unrelated to
Antigravity's source rotation review.


Antigravity received these concrete failures and the fixes in the existing
read-only review conversation and acknowledged the migration-discovery,
restored-schema and bounded-cleanup-context lessons. It owned no files and did
not run this verification.
