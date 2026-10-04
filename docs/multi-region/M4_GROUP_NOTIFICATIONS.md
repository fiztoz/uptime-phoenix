# M4 group notifications

A folder channel pages the folder's own incident. It is not copied onto every
monitor assigned to a remote probe. Direct monitor links stay on the source
that owns the assignment. This is the direct-monitor and group-notification
bullet of [M4](IMPLEMENTATION_PLAN.md).

## Behavior

Publication already omitted group links from the assignment graph. This slice
keeps that closure and adds the missing hub effect.

- A committed observation re-evaluates the folder after ingest returns. A
  permanently rejected observation does not. An incident, delivery, or
  condition event does not, because those do not change the availability
  evidence the folder rolls up.
- A newly applied current snapshot re-evaluates every active assignment it
  reconciled, including one omitted from the snapshot and marked missing.
  An identical snapshot retry does not page again.
- An ambiguous ingest whose cursor shows the prefix committed still
  re-evaluates. A rolled-back or conflicted write does not.
- Evaluation uses the same overall-policy rules as a local heartbeat. UNKNOWN
  does not clear a confirmed folder DOWN. The compare-and-set still admits
  one sender.
- The hub sends only `group_notifications`. The source keeps sending the
  monitor's own links. Public acknowledgement URLs stay off.
- The hook is optional. Local-only mode does not construct it. A probe worker
  attaches it to both ingest services; a failure to page does not fail the
  already-committed evidence.

## Production changes

| File | Change |
|---|---|
| `internal/core/services/group_alert_service.go` | `OnRegionalEvidence` shares the folder evaluator |
| `internal/core/services/probe_replay_service.go` | pages accepted observation monitors after commit |
| `internal/core/services/probe_state_service.go` | pages reconciled assignment IDs after commit |
| `internal/core/domain/probe_state.go` | receipt carries non-wire `MonitorIDs` |
| `internal/adapters/repository/probe_current_state.go` | fills those IDs only for a newly applied snapshot |
| `internal/bootstrap/run.go` | worker attaches the folder alerter when probes are enabled |

## Verification

Executed in this checkout (Go 1.26.6 via `GOTOOLCHAIN`, MariaDB 11 on
`127.0.0.1:43316` / `phoenix_ci`):

- `go build ./...` — pass.
- `go vet` on the changed packages — pass.
- `golangci-lint run` on the changed packages — 0 issues.
- `gofmt -l` on the changed files — empty.
- `go test -race -count=1 ./internal/core/services/ ./internal/core/domain/ ./internal/bootstrap/` — 833 passed.
- `TEST_MARIADB_DSN=…phoenix_ci… go test -race -count=1 ./internal/adapters/repository/ -run 'TestProbeCurrentStateAcceptance|TestRemoteConfigSyncContract'` — 30 passed, 0 failed. The DSN was set, so the MariaDB matrix was not skipped.

Named tests: `TestGroupAlert_RegionalEvidencePagesGroupChannelOnly`,
`TestProbeReplayServicePagesFolderAfterCommitOnly`,
`TestProbeStateServicePagesReconciledAssignmentsAfterCommit`,
`GroupChannelStaysOffAssignment`, and the current-snapshot receipt assertions
in `EmptyInitialSnapshotThenFirstEvidence` and `AheadOfBacklogAndDurableReceipt`.

Not run: `go test -race ./...`, `scripts/probe_runtime_smoke.py`. Local
acceptance does not authorize push, deployment, or a production migration.
