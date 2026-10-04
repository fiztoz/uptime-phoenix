# M4 capability advertisement

ICMP, Docker, database engines, and proxy bindings now have an explicit
runtime owner. An assignment the probe cannot execute is rejected before
activation. This is the capability bullet of [M4](IMPLEMENTATION_PLAN.md).

## Behavior

- `checker.ping.v1` is advertised only when the unprivileged ICMP socket opens.
  `checker.docker.v1` is advertised only when the operator configured a local
  Docker binding. Those gates were already accepted.
- The compiled proxy dialer supports `http`, `https`, and `socks5`. Hello
  advertises `proxy.<protocol>.v1` when an installed checker actually reads
  the proxy fragment (`http` or `s3`). There is no socket probe: an
  unreachable proxy remains a normal DOWN check.
- A remote assignment that references a proxy requires that protocol
  capability. `tcp`, `ping`, `dns`, `websocket`, `mqtt`, `rabbitmq`, `grpc`,
  `snmp`, `database`, and `docker` do not dial it, so publication and
  activation return `unsupported_capability` instead of ignoring the binding.
  Local checks are unchanged.
- Every supported database engine is compiled into `checker.database.v1`.
  Per-engine wire names stay off the contract, as decided in the pull-checker
  slice. An engine outside that set fails the installed validator before
  activation. `socks4` remains rejected.

## Production changes

| File | Change |
|---|---|
| `internal/adapters/checker/capability.go` | proxy-capable types and protocol names |
| `internal/adapters/probe/checker_capabilities.go` | hello advertisement for those protocols |
| `internal/adapters/probe/config_encoder.go` | remote proxy assignments require the protocol capability |
| `internal/adapters/probe/edge_config.go` | activation rejects a proxy the checker would ignore |
| `internal/adapters/probe/config_validation.go` | installed checkers must support a required proxy capability |
| `cmd/probe/runtime.go` | hello includes the proxy advertisement |

## Verification

Executed in this checkout with `GOTOOLCHAIN=go1.26.6`:

- `go test -count=1 ./internal/adapters/probe/ ./internal/adapters/checker/ ./internal/adapters/scheduler/ ./cmd/probe/` — 1598 passed, 0 failed.
- `go vet` and `golangci-lint run` on those packages — 0 issues. `gofmt -l` on the changed Go files — empty.
- Named tests: `TestProxyCapabilitiesFollowInstalledProxyCheckers`, `TestRemoteConfigEncoderAdvertisesExecutableProxy`, `TestRemotePublicationRejectsUnsupportedDatabaseEngine`, and the existing edge activation tests for the HTTP fixture.

Not run: `go test -race ./...`, MariaDB matrix, `scripts/probe_runtime_smoke.py`. Local acceptance does not authorize push, deployment, or a production migration.
