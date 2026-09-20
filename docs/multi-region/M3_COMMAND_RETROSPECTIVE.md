# Durable command integration retrospective

Scope: the ACK integration following `7fee89e`, on
`codex/multi-region-probe-plan`. These findings came from Codex's integration and
test work. They are not failures attributed to Antigravity or the earlier agent.
The full milestone still requires rotation, reset and final partition acceptance.

## Summary

The command queue now preserves exact protected requests, current session fencing
and independent source results. Replay must correlate a new ACK with its issued
command while permitting either telemetry or the control result to arrive first.
An accepted opening continues to identify the original incident after reassignment;
it never authorizes acknowledgement of a later outage.

Three verification failures exposed assumptions at different boundaries: MariaDB
index ownership during downgrade, fault-trigger cleanup between database cases,
and a transport test that omitted the hub's retained configuration. Each was
reproduced and corrected without removing its production guard.

## MariaDB downgrade index dependency

**Symptom and reproduction.**
`TestProbeCommandStorage/mariadb/MigrationGuards` failed with error 1553 when
dropping `idx_probe_commands_retention`: MariaDB needed it for the existing
`probe_id` foreign key. SQLite's same migration case did not expose this.

**Root cause.** Migration 036 relied on InnoDB's implicit supporting index for
the foreign key. Migration 061 added covering indexes starting with `probe_id`.
InnoDB could replace the old implicit index with one of those covering indexes.
The downgrade then tried to remove the only remaining FK-supporting index.
MariaDB DDL had already dropped the first index before failing the second drop;
the test database therefore retained a partial downgrade.

**Fix.** Both MariaDB directions ensure an explicitly named
`idx_probe_commands_probe_id` before changing the new covering indexes. The
downgrade keeps that supporting index and uses `DROP INDEX IF EXISTS` for retry
after a partial DDL failure. The retained-command data guard still runs first.
SQLite keeps its own engine-specific migration.

**Validation.** The original migration regression passed on MariaDB and SQLite,
including refusal with retained protected commands and a safe empty down/up cycle.
The focused command-storage rerun recorded 23 named passes, including 11 MariaDB
parent/subtest events, with no skips or failures.

**Prevention.** Inspect actual FK/index metadata on the production engine, and
exercise down/up effects. An engine can substitute a dependency even when the
original migration did not name it explicitly. Treat MariaDB DDL as capable of
partial completion; do not assume a surrounding test transaction rolls it back.

## Database fault injection leaked into the next case

**Symptom and reproduction.** The first ACK replay matrix passed the injected
rollback case, then MariaDB's `ServiceIssuanceRetries` setup failed with an
internal error. The prior fault trigger was still installed on `probe_streams`.

**Root cause and fix.** The existing `injectOutboxFailure` helper returns a
cleanup function. The new caller discarded it. The corrected caller defers that
function immediately; the leftover trigger was removed only from the disposable
test schema before retrying. No application error handling was relaxed.

**Validation.** The complete ACK replay matrix then passed on both engines:
25 named parent/subtest events, including 12 MariaDB events, zero skips/failures.
It exercised telemetry-before-result, result-before-telemetry, forged actor/note,
unissued/unsent commands, foreign stream, contradictory result, corrupted protected
request, late rollback, competing issuance and preserved ACK recovery.

**Prevention.** Register cleanup at creation for any database fault injector.
An isolated fault assertion is insufficient when the next test shares a server.

## Invalid transport fixture hit the real rollback guard

**Symptom and reproduction.**
`TestCommandRuntimeExactBytesReceiptLossAndRestart` initially reported no source
result. Tracing showed `probe handshake rejected` before any command claim.

**Root cause.** The test preloaded edge configuration revision 12 but passed no
configuration document to `HubTransport.Run`. The hub therefore advertised desired
revision zero. `ValidateHandshake` correctly refused a hub behind the edge's
accepted revision; this was not a command execution or result-storage defect.

**Fix and validation.** Supply the same retained document and its real execution
capabilities in the fixture. The race-enabled TLS test then passed interrupted
application, failed result persistence, edge store/runtime reopening and retry
after expiry. The recovered outcome matches the original, the source counter
contains exactly one ACK transition, and a changed raw payload digest conflicts.
The test's hub dispatcher is a controlled test double; real hub storage has its
separate two-engine contracts, and the compiled-process test covers their wiring.

**Prevention.** Trace the first failed boundary before interpreting a missing
downstream receipt. A fixture must satisfy the complete handshake/config contract.
Never remove rollback protection to make a command test reach its target branch.

## Delegation lessons

Antigravity's assignment is bounded and read-only: no source ownership, no project
rule edits, and no claimed test execution. Codex supplies the actual code and
contracts, verifies reported findings, runs the effects-based tests and owns the
commit. Workflow names such as `/learn` do not expand file ownership or authorize
unrelated edits. Prior audit corrections remain relevant: command results are
recoverable control frames, and every retry uses current session authority while
preserving its immutable payload identity.

The final integration audit initially raised three unsupported findings. Actual
callee evidence showed that stream IDs are durable epochs, replay facts are built
from the event's retained revision, and the shared command parser rejects a null
assignment generation before dereference. Antigravity withdrew all three after
receiving that evidence. The lesson is to trace the constructor/validator contract
before proposing weaker authorization or redundant panic guards.

The review is evidence about inspected paths. The build, real-engine matrix,
transport regression and compiled process run are separate evidence; none alone
proves complete M3 acceptance. See the command acceptance record for the final
commands, counts, exact hashes and remaining limitations.
