# M8 — verified SSH host preflight

Status: **first bounded M8 slice implemented; SSH installation is not implemented**.
Baseline: merged M7 at `8c95ae7`. This work does not close the outstanding M6
operational/release gates, authorize deployment, or enable aggregate delivery.

## Scope and decision

M8 requires asynchronous, idempotent installation which succeeds only after
verified runtime enrollment and configuration activation. An SSH process exiting
zero is not that evidence. Start with the prerequisite that can be delivered
honestly on its own: an operator-only, nonmutating SSH compatibility preflight.

`phoenix-probe-admin inspect-host` runs before hub environment loading. It opens
no hub database, creates no registration or administrative operation, and adds
no HTTP route, UI, worker, feature flag, migration, or startup dependency. Existing
probe-admin commands retain their existing hub-backed flow. Only the operator
running this local command authorizes its use; this is not a remotely accessible
substitute for the future admin-only installation API.

The new `ports.ProbeHostInspector` is discovery-only. It deliberately exposes no
arbitrary-command method. `ProbeHostPreflightService` applies platform/runtime/
privilege compatibility rules without importing SSH, filesystem, or DB adapters.
The SSH adapter reuses the existing `probe.EndpointPolicy` and
`probe.ValidateDestination` rather than creating a second destination policy.
`golang.org/x/crypto/ssh` and `knownhosts` use the already-pinned, CGO-free
`golang.org/x/crypto` module; no dependency was added.

## Operator usage

Obtain host trust over an authenticated out-of-band channel first. An unverified
`ssh-keyscan` result is **not** sufficient. Use either a verified OpenSSH
`known_hosts` file or a verified canonical OpenSSH `SHA256:BASE64` host-key
fingerprint, never both. There is no trust-on-first-use or automatic replacement.

```sh
go run ./cmd/phoenix-probe-admin inspect-host --help

go run ./cmd/phoenix-probe-admin inspect-host \
  --address edge.example:22 \
  --user deploy \
  --identity-file /secure/temporary-deploy.key \
  --known-hosts /secure/verified_known_hosts
```

Flags:

| Flag | Contract |
|---|---|
| `--address` | Explicit DNS name/IP and canonical numeric port; bracket IPv6. No URL, username, path, scoped IPv6 zone or implicit port |
| `--user` | Nonempty bounded SSH login name using ASCII letters, digits, `_` or `-` |
| `--identity-file` | Temporary, unencrypted deployment private key; private regular file, no symlink, at most 64 KiB |
| `--host-key-sha256` | Verified canonical OpenSSH SHA-256 fingerprint (unpadded base64), not the runtime TLS certificate fingerprint |
| `--known-hosts` | Alternative verified OpenSSH trust file; regular file, no symlink, at most 1 MiB. Host/port matching, hashed names and revoked-key rejection use `knownhosts` |
| `--endpoint-policy` | Optional existing JSON `allowed_cidrs` policy format. No environment policy is implicitly loaded |

No SSH agent, SSH config, password/passphrase prompt, environment proxy,
forwarding, PTY, or enrollment/runtime credential is used. The raw key buffer is
cleared after the command; the parsed signer remains in process memory for the
command's lifetime. No credential is written to hub state. The operator remains
responsible for protecting/removing their temporary key file and revoking its
remote authorization after use.

Public unicast destinations are allowed by default. Private, loopback and other
nonpublic ranges require an explicit matching CIDR. Metadata, link-local,
unspecified and multicast addresses remain forbidden even with a broad CIDR.
DNS is resolved once and only individually permitted **numeric IPs** are dialed;
the original hostname and port remain the `known_hosts` identity. This prevents
a second DNS lookup from substituting a forbidden destination.

## Exact effect and output

After host-key verification and public-key authentication, one fixed POSIX-shell
script discovers:

- `uname -s` and `uname -m`;
- presence of the Docker CLI (**not** daemon health or access);
- presence of `systemctl` **and** `/run/systemd/system`;
- root, or whether noninteractive `sudo -n -- /bin/true` succeeds.

There are no package operations, file/unit/container/volume creation, Docker
context connections, updates, cleanup, service restarts, or runtime listener
probes. Normal remote SSH/sudo audit logging still applies. Testing permission
for `/bin/true` does **not** establish permission for every future installer
command under a restricted sudoers policy. Installation must check its own
required privileges and fail honestly.

Supported compatibility results are Linux `x86_64` → `amd64`, and Linux
`aarch64`/`arm64` → `arm64`, with root/noninteractive sudo and either a running
systemd marker or an existing Docker CLI. Other platforms, including ARMv7,
are explicitly refused in this slice. A clean Linux systemd host need not have
Docker. An existing Docker CLI does not establish that containers can be run.

Example explicit JSON view (one result, no terminal stream):

```json
{"state":"preflight_passed","os":"linux","architecture":"amd64","docker_cli":false,"systemd":true,"privilege":"sudo"}
```

Exit `0` means **only these preflight checks passed**, never that a probe was
installed, enrolled, reachable on its runtime port, or running activated config.
Invalid invocation exits `2`; failed discovery/compatibility/trust/key/policy,
cancellation or timeout exits nonzero with no success JSON. Errors are fixed
nonsecret diagnostics. Raw SSH/library errors, flags/arguments, host banners,
stdout/stderr and credentials are not emitted. Domain structs are not serialized.

A 30-second context/deadline covers DNS, connect, SSH negotiation, channel/exec
requests and execution. Cancellation closes the transport, including during
handshake or a peer which never accepts/finishes a command. Combined stdout and
stderr have one 4096-byte budget; overflow closes the transport immediately.
Only a strict versioned discovery record is parsed; stderr is discarded.

## Implementation map

- `internal/core/domain/probe_provisioning.go`: pure target/facts/result types,
  syntax validation and fixed diagnostics.
- `internal/core/ports/probe_provisioning.go`: discovery-only inspector port.
- `internal/core/services/probe_host_preflight.go`: compatibility use case.
- `internal/adapters/provisioning/ssh.go`: trusted SSH connection and discovery.
- `internal/bootstrap/probe_ssh_preflight.go`: local composition and explicit CLI
  view; `cmd/phoenix-probe-admin/main.go` dispatches before hub startup.
- Adjacent domain/service/adapter/bootstrap tests and
  `cmd/phoenix-probe-admin/ssh_preflight_test.go`: policy, transport and actual
  process-entry coverage. All keys are generated inside temporary test fixtures.

## Verification and limits

The focused race command is reproducible without MariaDB, an installed SSH
server, an SSH agent, real credentials or an external host:

```sh
GOTOOLCHAIN=go1.26.9 go test -race -count=1 \
  ./cmd/phoenix-probe-admin ./internal/bootstrap \
  ./internal/adapters/provisioning ./internal/core/domain ./internal/core/services \
  -run 'Test(ProbeAdminSSHPreflight|ProbeSSHPreflight|SSHPreflight|ProbeHostPreflight|ProbeSSHHost)'
```

Tests exercise the actual SSH protocol against an ephemeral loopback server,
the fixed script through `/bin/sh` with controlled command fixtures, and the
actual CLI in child processes with deliberately invalid hub environment. They
cover verified pins, hashed `known_hosts` and original DNS/port identity,
unknown/wrong/revoked keys rejected **before** authentication, rejected auth,
metadata/private-address policy, frozen caller policy, strict/failed discovery,
shared output limits, missing sudo, unsupported architecture, systemd and Docker
compatibility, cancellation in handshake/channel/exec-accept/execution, private
input failures, and nonsecret output/error behavior.

Executed 2026-10-16 using Go 1.26.9 on the frozen Go source (baseline `8c95ae7`;
source manifest retained with the private artifacts):

| Command / evidence | Executed result |
|---|---|
| `go build ./...` | Passed |
| `go vet ./...` | Passed |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` | Passed |
| `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build ./...` | Passed |
| `go test -race -count=1 -timeout=2400s -json ./...` | Passed: 23 test packages, 5,470 named passes, zero failures; 488 named skips (483 MariaDB-dependent, five other opt-in/helper cases). Thirteen additional packages have no tests |
| Focused command above, final control | 61 named passes, zero failures/skips |
| `go test -race -count=20 -json ./internal/adapters/provisioning` | 720 named passes, zero failures/skips |
| Five isolated Go overlay assertion mutations | All caught: disabled trust, disabled shared output budget, hostname re-dial, accepted unsupported architecture, disabled cancellation closure. No compile-error/timeout/panic/race false positives; unmodified control passed |
| `gofmt -l internal/ cmd/` and `git diff --check` | Clean |

`golangci-lint` was unavailable; the documented `go vet`/`gofmt` fallback was
executed. `govulncheck` was also unavailable and was not run. No vulnerability or
full `make gate-full`/release acceptance is claimed. Frontend/Helm gates were not
run for this backend-only slice. There are no dependency or schema changes.

Engines exercised by the full backend suite: existing SQLite fixtures; **no real
MariaDB** (`TEST_MARIADB_DSN` deliberately unset). A passing parent or SQL-shape
test with MariaDB in its name is not evidence of a live-engine execution. The
five other skips are the MongoDB real-server opt-in, the existing Telegram
severity case, two helper-only process entrypoints and the Linux disk-full opt-in.
None is an M8 test. The SSH fixtures use loopback only; no real host, existing
credential, SSH agent or external SSH server was used.

Local private artifacts live under `tmp/m8-ssh-preflight-verification/`;
`run-03/summary.json`, `source-manifest.json` and `result.json` hold final results.
The first launcher failed before compilation because the Go wrapper refused
cached toolchain verification with `GOSUMDB=off`; the rerun invoked the installed
Go 1.26.9 binary directly. The superseded `run-02` was intentionally interrupted
after adding context-first trust/key construction, to free resources for the
final run. All partial logs are preserved separately. Neither the failed
launcher, interrupted run nor read-only review is complete-suite pass evidence.
The Go source remained unchanged across final gates, overlay mutations and the
unmodified control.

This is **not** the full M8 acceptance matrix. No real VM, actual sudoers policy,
Docker daemon, systemd service installation, runtime port, enrollment or config
activation is verified by these preflight fixtures. MariaDB is not needed for
this no-storage slice; skipped existing MariaDB tests must not be reported as
production-engine coverage. No frontend or Helm files change.

## Next bounded M8 work

1. Freeze the durable admin-only asynchronous operation API, idempotency and
   worker/interruption/cancellation contract. Keep ephemeral SSH key material
   outside the hub DB; define explicit resubmission after credential loss rather
   than silently claiming resumability.
2. Add a version/digest-pinned artifact contract and integrity verification, then
   a complete supported installer path (including non-Docker systemd), protected
   persistent identity/data/secret files and a per-operation resource journal.
   Reruns must preserve identity and unrelated host resources; cleanup must be
   explicit and only act on proven owned resources.
3. Add bounded redacted phase progress and failure/exit evidence. Do not expose
   a generic terminal or persist raw remote logs containing secrets.
4. Complete only after authenticated runtime enrollment **and exact successful
   config activation**. Prove this through production entry points, including
   inaccessible runtime ports, interruption, existing install, wrong platform,
   bad trust and insufficient privilege. Update/rollback remains separate.
5. Keep all M6 operational/release and operator-owned deployment approval gates.
