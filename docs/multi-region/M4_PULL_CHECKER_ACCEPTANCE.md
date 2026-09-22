# M4 pull-checker coverage acceptance — first slice

Date: 2026-09-22. Scope: [implementation plan M4](IMPLEMENTATION_PLAN.md),
bullets 1 and 2 partially — remote execution of every pull monitor type that
needs no probe-local resource, and honest capability advertisement. This is
not M4 completion: docker resource bindings, lifecycle/auxiliary state
(TLS expiry, capacity), escalation, group/status-page recovery, Insights
projection versions, backup/restore compatibility and deployment documentation
remain open.

## Accepted behavior

- The remote runtime accepts every pull monitor type except docker:
  `http`, `tcp`, `ping`, `dns`, `websocket`, `mqtt`, `rabbitmq`, `grpc`,
  `snmp`, `database`, `s3`. Push stays local-only (M9). Docker assignments
  fail publication and edge activation explicitly with
  `unsupported_capability` until assignments can carry advertised binding
  keys; a hub filesystem/socket reference is never sent to pretend a
  VM-local Docker resource exists.
- The probe hello advertises `checker.<type>.v1` only for checkers the build
  actually contains, derived from the installed checker registry instead of
  a hardcoded M2 list. `checker.ping.v1` is advertised only when the exact
  unprivileged `udp4` ICMP socket the ping checker opens can be bound (no
  packet is sent); `checker.docker.v1` is withheld. Database engines are
  compiled into every build, so `checker.database.v1` is the honest
  per-engine claim; per-engine capability names were deliberately not added
  to the wire contract.
- Impossible assignments are rejected before activation by the existing
  atomic design: the hub session handshake requires every capability named
  by the desired snapshot, the edge config transfer revalidates the union
  against its own advertisement, and edge activation validates each
  assignment against installed checkers. One impossible assignment blocks
  the session and keeps the probe on its last accepted configuration —
  nothing is silently dropped.
- Local-only deployment is unchanged: no new external dependency, no
  migration, no new monitor/provider/permission dimension. The only new
  import is `golang.org/x/net/icmp` from the already-direct
  `golang.org/x/net` module (CGO-free).

## Production changes

| File | Change |
|---|---|
| `internal/adapters/checker/capability.go` | `RegisteredPullTypes` (registry-derived inventory) and `ICMPAvailable` (exact-socket probe) |
| `internal/adapters/probe/checker_capabilities.go` | `PullCheckerCapabilities` wire advertisement derivation |
| `internal/adapters/probe/edge_config.go` | `validateEdgeRuntimeSnapshot` accepts all pull types; docker/resource bindings still rejected |
| `cmd/probe/runtime.go` | capability list built from the registry plus ICMP gating instead of hardcoded http/tcp/dns |
| `scripts/probe_runtime_smoke.py` | second monitor of type `websocket` through the real API/assignment/encode/transfer/decode/execute/ingest pipeline |

## Verification evidence

Commands executed (this checkout, Go 1.27.1 local / GOTOOLCHAIN=go1.26.6 for
the contract runs; disposable MariaDB 11.8.9 container `phoenix-m04-checker-coverage`,
port 43326, fresh `phoenix_ci` and `phoenix_smoke` schemas):

- `go build ./...` — pass.
- `go test -race -count=1 ./...` — pass in 22/23 packages;
  `internal/adapters/repository` passes with an explicit longer timeout
  (`-timeout 2400s`, 629s actual, 3,719-class suite unchanged). The default
  10-minute local timeout on this machine predates this slice and reproduces
  on the clean tree; it is a local performance issue, not a code failure.
- `golangci-lint run` (whole repo) — zero warnings. `gofmt -l` — empty.
- `GOTOOLCHAIN=go1.26.6 TEST_MARIADB_DSN=…phoenix_ci… go test -race -count=1
  ./internal/adapters/repository ./internal/adapters/probe
  ./internal/core/services -run 'LocalConfig|ProbeConfig|ConfigSnapshot|
  ConfigTransfer|ConfigInspector|RemoteConfig|EdgeConfig|PullCheckerCapabilities'`
  — 229 passed, both engines (SQLite + real MariaDB).
- `… -run TestRemoteConfigSyncContract` — pass on sqlite and mariadb subtests
  (hub remote snapshot publication against real MariaDB).
- `scripts/probe_runtime_smoke.py` with `--mariadb-container` — 16 PASS marks
  including new `extended pull checker telemetry reaches the hub`; both
  monitors healthy, exact incident/delivery counts for the HTTP monitor
  unchanged. Evidence at `/private/tmp/m4-smoke-out` (local artifact).

Named new tests exercised:

- `internal/adapters/probe`: `TestEdgeConfigDecoderAcceptsEveryPullCheckerType`
  (11 types through the installed validators), docker rejection case in
  `TestEdgeConfigDecoderRejectsUnsupportedWithoutIO`,
  `TestRemoteConfigEncoderPublishesEveryPullCheckerType`,
  `TestRemoteConfigEncoderRejectsUnsupportedWork/docker`,
  `TestPullCheckerCapabilities*`.
- `internal/adapters/checker`: `TestRegisteredPullTypesReflectsRegistry`,
  `TestICMPAvailableMatchesSocketReality`.
- `internal/adapters/scheduler`: `TestEdgeSchedulerExecutesExtendedPullCheckerType`
  — a websocket monitor executes through the production EdgeScheduler against a
  real local WebSocket server and commits a durable `UP` observation.

Acceptance criteria still unverified:

- Docker resource-binding flow end to end (hello bindings, assignment binding
  keys, migration, probe-local socket resolution) — follow-up slice.
- Ping on a host where unprivileged ICMP is permitted (this macOS host cannot
  bind the socket; the advertisement path is unit-tested and the socket probe
  is the same syscall pro-bing uses).
- The remaining M4 bullets (lifecycle/auxiliary state, escalation, recovery
  policy, Insights versions, backup/restore, operator docs) — see
  [status](IMPLEMENTATION_STATUS.md).
