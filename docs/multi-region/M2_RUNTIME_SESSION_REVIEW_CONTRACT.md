# Antigravity runtime integration review

Use Gemini 3.8 Flash High. This replaces the earlier runtime implementation
assignment: Codex implemented it while the Gemini quota was exhausted.

Ownership: read source and run focused checks, but edit only
`docs/multi-region/M2_RUNTIME_SESSION_REVIEW.md`. Codex owns all source, tests,
bootstrap, CLI, scripts and other documentation during this review. Do not revert,
reformat or overwrite those files. Do not commit, push, deploy or change models.

Read AGENTS.md, the M2 section of IMPLEMENTATION_PLAN.md and the protocol session
and enrollment contracts. Inspect:

- `internal/adapters/probe/runtime_session.go` and its tests;
- `internal/adapters/probe/hub_transport.go`;
- `internal/core/services/probe_connector_service.go` and its tests;
- `cmd/probe/runtime.go`;
- `internal/adapters/repository/probe_connection.go` and `probe_connector.go`.

Trace actual shutdown, failed lease renewal, a lost enrollment receipt, a stale
session close, higher-generation replacement, and configuration transfer rejection.
Check that authenticated does not mean ready or config applied, and that callbacks
cannot accidentally grant stale authority. Report actionable issues with exact
paths, mechanism and a reproducer. Separate observed bugs from untested concerns.
No M2 completion claim: Codex will run process tests and the full integration gate.

Test invocation: prefix shell commands with `rtk`; use `GOTOOLCHAIN=go1.26.6` and
`GOCACHE=/private/tmp/phoenix-go-cache-template-race`. Do not run MariaDB suites
concurrently: Codex owns the shared disposable database. Focused probe/core tests
may run without TEST_MARIADB_DSN. Record exact commands and results, including skips.

Learning follow-up: read `docs/guides/m01-runtime-verification.md` and
`docs/postmortems/2026-09-20-m1-integration-followup.md`. Your earlier broad UPDATE
fault trigger fired on the transaction's initial no-op writer lock, before the
source inserts. It did not prove rollback after those inserts. Codex added
`UPDATE OF last_created_seq ... WHEN NEW.last_created_seq != OLD.last_created_seq`
to target the final counter write, plus queue-headroom reservation tests. Explain
this distinction in your review, and apply it to every failure-injection claim.

Deliver a concise evidence report and stop. Do not resume the obsolete runtime
implementation task from a pending draft or earlier conversation message.
