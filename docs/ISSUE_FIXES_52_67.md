# Issues #52–#67: fixes and verification

Local verification completed 2026-10-06 against the working tree based on
`a9d7354`. This record does not claim a pushed commit, GitHub CI execution,
issue closure, release publication, or deployment.

## Fixes

| Issue | Change | Regression evidence |
| --- | --- | --- |
| #52 | gRPC TLS uses real TLS credentials with certificate and hostname validation. | `grpc_test.go`: local TLS health server, trusted/untrusted certificates, wrong hostname and NOT_SERVING. |
| #53 | DNS authority SOA does not prove an unrelated requested record exists. Requested-type and owner checks preserve intended SOA/NS behavior. | `dns_test.go`: NODATA A/AAAA/MX, authority SOA/NS, CNAME and expected-value cases. |
| #54 | RabbitMQ hostname configuration passes a decoded vhost to URL.Path, avoiding double escaping. | `rabbitmq_test.go`: URI round trips plus the actual AMQP connection.open vhost. |
| #55 | Session shutdown records a first-wins termination cause before teardown; an internal close cannot return success. | `session_close_test.go` and existing session health tests, with race detection. |
| #56 | All publication paths require a completed successful CI run and all required jobs for the exact release SHA. Prepare pins that SHA once; every binding rejects a moved tag. | `scripts/release/test-ci-gate.py`: real extracted scripts with temporary tagged repos and mocked GitHub API. Wired into `make release-image-gate`. |
| #57 | Monitor detail prefers live aggregate health, maps mixed-source heartbeat display to overall status, and refreshes history on monitor.health. | `monitor-detail-state.test.ts`; `issue-live-monitor.spec.ts` browser badge/history/timeline assertions. |
| #58 | Ordinary edit requests omit active; only explicit pause/resume changes activation. | `monitor-form-pause-preserve.test.ts`; real browser/API paused-edit regression. |
| #59 | gRPC form uses the actual checker keys: url, service_name, tls and timeout; legacy hostname/service values migrate on edit. | `monitor-form-grpc-keys.test.ts`; browser create/reopen/save round trip. |
| #60 | Region editor exposes its validated draft; monitor creation sends the initial assignment set in one POST, with no post-create assignment compensation. | `monitor-form-create-assignments.test.ts`; browser remote-only create, real rejected assignment/no leftover, retry and older fleet regression. |
| #61 | Probe status matches id; config status matches probe_id, on both detail and fleet list. | `probe-detail-events.test.ts`; browser request-count and unrelated-probe assertions. |
| #62 | Chart time domain advances monotonically when an accepted payload arrives and includes its newest data. | `utils/chart.test.ts`; browser geometry assertion after a refreshed chart payload. |
| #63 | Migration 075 adds lease_epoch. Queued checks carry immutable lease identity; heartbeat commits validate stored owner/epoch/expiry transactionally. Refresh cannot revive expired leases, including across lock waits. | `local_heartbeat_fence_test.go`, `monitor_lease_epoch_test.go`, `monitor_lease_clock_test.go`, service and scheduler fencing tests. SQLite and MariaDB executed. |
| #64 | Group parent cycles, including self-cycles, fail validation and defensive ordering before any writes. | `configascode_groupcycle_test.go` and `configascode_grouporder_test.go`, including isolated fatal-recursion regression. |
| #65 | Backup restore bypasses new-monitor default notification attachment and restores the exact backed-up links/flags. | `backup_notification_links_test.go`; `TestBackupImportNotificationLinkFidelity` through production services on both engines. |
| #66 | Push token config and lookup column stay synchronized. Tokenless/redacted creates generate a usable token; updates preserve existing values. Config export redacts tokens. | Push-token service tests; `TestConfigApplyPushLookupLifecycle` through production config apply and real lookup on both engines. |
| #67 | A deterministic, per-extension pod-template checksum changes when its chart-managed UI token rotates. | `scripts/issue67-ext-token-checksum.sh` renders unchanged/rotated/tokenless/external-secret cases; wired into `make helm-validate`. |

## Related integration corrections

- Config-as-code comparisons and updates normalize omitted weight and accepted
  status-code defaults consistently with creation. Real-engine tests now prove
  repeated apply is a no-op, including generated push tokens.
- Migration concurrency verification counts the embedded migration set rather
  than retaining the historical hard-coded count of 74.
- Lease renewal tests age a still-valid lease before asserting a changed-row
  count: MariaDB can legitimately return zero for same-second renewals.
- The older fleet browser test now injects a failure at the atomic create POST,
  asserts zero persisted monitors, and retries without changing the selected set.

## Executed verification

Go commands used `GOTOOLCHAIN=go1.26.6`. The database was a fresh local,
localhost-bound MariaDB `11.8.9-MariaDB-ubu2404` container (`mariadb:11`, image
digest `sha256:6422478cb8e159f080fb1d8ccf65101e26fe51385787fde7d16c3b165a331f15`)
with a disposable tmpfs data directory, not an existing installation. The test
container was removed after verification. No credentials are recorded here.

| Command / gate | Result |
| --- | --- |
| `go test -race -json -count=1 -timeout 2400s -p 4 ./...` with disposable MariaDB configured | Exit 0; 22 packages passed; 4,484 named test/subtest pass events, zero failures, five skips. |
| MariaDB coverage within that run | 359 named `/mariadb` passes, no MariaDB skips; includes lease expiry/renewal/reacquisition/lock waits, migration up/down, config push lookup and backup link fidelity. |
| `go build ./...` | Passed. |
| `golangci-lint run` and `gofmt -l internal/` | Zero findings; formatting clean. |
| `govulncheck ./...` | Exit 0; no reachable vulnerabilities. Reported one uncalled package vulnerability and one uncalled required-module vulnerability. |
| `cd web && bun run check` | Zero errors and zero warnings. |
| `cd web && bun run test` | 324 passed, zero failed. |
| `cd web && bun run build` and `bun run lint` | Passed. |
| `cd web && bun run test:e2e` | 27 passed, zero failed. |
| `make release-image-gate m6-backend-harness-gate` | Passed; release behavior is tested with mocked external tools/API. |
| `make helm-lint helm-validate` | Passed; render-level verification, not a live rollout. |
| `actionlint -shellcheck=` and `git diff --check` | Passed. |

The five Go skips are: optional real MongoDB server coverage, the existing
Telegram hard-coded-URL test, opt-in edge disk-full coverage, and two helper
process entry points (their parent tests execute the subprocess scenarios).

An earlier run spanning host suspension was **not** accepted as passing: it
contained timing failures plus the stale migration-count assertion. The
uninterrupted final run above passed after the fixture correction. The first
full browser run likewise exposed the obsolete post-create-PUT test; the
updated full run passed. Earlier failed runs are not represented as successes.

## Boundaries and rollout notes

- Browser tests use the real Go application and disposable SQLite. The live
  monitor spec deliberately injects WebSocket frames and selected history/chart
  responses; real remote-probe transport is not established by those tests.
- Release publication and live GitHub API behavior were not exercised. The
  release gate's failure handling and SHA binding were executed against fixtures.
- Extension rollout/authentication on a Kubernetes cluster was not executed.
  Externally managed Secret contents remain opaque to Helm and require an
  operator-triggered restart; chart-managed token rotation changes the template.
- Deploy migration 075 with the new code. Drain/stop old sharded workers before
  relying on the lease fence: an old binary can still write unfenced results.
  New workers must also be stopped before downgrading away their epoch column.
- No application dependencies were added. Commit/PR publication and issue
  disposition are tracked on GitHub; this record establishes local verification
  only, not remote CI success or deployment.
