# M8 — one-time SSH password host bootstrap

## Goal

The [restricted-key installer](M8_INSTALL_API.md) never accepts a password. Before
it can install a probe, every host needs a one-time root setup (see
[TESTING.md](../TESTING.md#m8-host-authorized-systemd-actions-data_directory-unit_file-service)):

1. the helper `/usr/local/bin/phoenix-probe-admin`;
2. the hub's stage-authority public key at `/etc/phoenix-probe/stage-authority.pub`
   ([key provisioning](KEY_PROVISIONING.md#stage-authority-key-m8-binary-staging-grants));
3. a restricted forced-command entry for the deployment key in root's
   `authorized_keys` ([authorized-dispatch](M8_HOST_ACTIONS.md#one-forced-command-authorized-dispatch)).

The bootstrap does exactly these three steps from the hub, using an SSH login
password **once**. It then verifies the restricted key. After that, every install
uses the restricted key only: the installer, its preflight and its contracts are
unchanged.

## Host-key contract: pinned only

The bootstrap follows the [SSH preflight](M8_SSH_PREFLIGHT.md) trust rule: an
operator-verified canonical `SHA256:` host-key fingerprint is required. There is
**no trust-on-first-use** and no fingerprint discovery endpoint.

The pin is checked inside the SSH key exchange, before user authentication starts.
On a mismatch the hub retries once, restricted to the host-key family that presents
the pinned key, as the installer does. **A host that does not present the pinned
key never receives the password** (`host_trust_failed`). Read the fingerprint on
the host through a channel you already trust, for example a cloud console:

```sh
ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
```

## API

`POST /api/probe-host-bootstraps`. The route is always registered and is guarded by:
- `SessionOrAPIKey(write)`, then `RequireAdmin`;
- then a per-IP credential rate limit (scope `probe-host-bootstrap`): 5 attempts a
  minute, shared through Redis when `REDIS_URL` is set. Only callers that already
  passed the admin gate spend this budget.

It is enabled with the installer (`PROBE_SSH_INSTALL_ENABLED=true`, API/all mode
only). It uses the installer's `PROBE_STAGE_AUTHORITY_KEY_FILE` (public half),
`PROBE_INSTALL_ARTIFACT_DIR` and `PROBE_ENDPOINT_POLICY_FILE`. Responses are
`Cache-Control: no-store`.

The request is **synchronous**. It is one strict JSON object of at most 128 KiB.
Unknown, duplicate, case-variant, null or mistyped members are refused, and so is
trailing JSON:

```json
{
  "ssh_address": "probe-host.example.com:22",
  "ssh_user": "ops",
  "ssh_password": "…",
  "host_key_fingerprint": "SHA256:…",
  "private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\n…",
  "helper_version": "0.6.0",
  "helper_architecture": "amd64",
  "helper_sha256": "<64 lowercase hex>",
  "helper_size": 45678752
}
```

- `ssh_password` is 1..1024 bytes with no NUL, CR or LF. When `ssh_user` is not
  `root`, the same password is the `sudo -S` password.
- `private_key` is the **deployment key** (≤ 64 KiB, unencrypted), the same key
  later passed to `POST /api/probe-installs`. Its public half becomes the restricted
  entry; the private half only verifies that entry.
- The `helper_*` fields pin the release file `phoenix-probe-admin_<version>_linux_<arch>`
  in `PROBE_INSTALL_ARTIFACT_DIR`, as `binary_*` pins the probe for installs. Before
  any connection the hub checks:
  - size and SHA-256;
  - a static, CGO-free Linux ELF of the pinned architecture;
  - Go build info naming `cmd/phoenix-probe-admin`. The probe binary is not accepted
    as the helper.

The 200 response is a view, never a domain type:

```json
{"kind":"ssh_probe_host_bootstrap","status":"verified",
 "helper":{"version":"0.6.0","architecture":"amd64","state":"installed"},
 "stage_authority":"installed","authorized_key":"added"}
```

`state`/`stage_authority` is `installed` or `unchanged`; `authorized_key` is `added`
or `unchanged`. A 200 is only returned after verification succeeded.

| Case | Status / code |
|---|---|
| Malformed body, address, user, pin, password, key or helper pin | 400 `invalid_bootstrap` |
| Body over 128 KiB | 413 `bootstrap_too_large` |
| Helper file missing or failing its pin | 422 `artifact_rejected` |
| Host key does not match the pin (password **not** sent) | 422 `host_trust_failed` |
| Endpoint policy refuses the resolved address | 422 `host_destination_refused` |
| TCP or handshake failure | 422 `host_unreachable` |
| Login user or password rejected | 422 `host_password_rejected` |
| Kernel is not Linux / `uname -m` does not match the helper pin | 422 `host_unsupported_os` / `host_architecture_mismatch` |
| `sudo -S` refused (wrong password, no sudo rights, `requiretty`) | 422 `host_privilege_failed` |
| A destination dir/file not owned by root, group/world writable, or a directory where the helper goes | 422 `host_layout_unsafe` |
| A different stage-authority key is already trusted | 422 `host_authority_conflict` |
| The deployment key already appears in root's `authorized_keys` with other options | 422 `host_authorized_key_conflict` |
| Helper digest mismatch on the host | 422 `helper_transfer_failed` |
| Root did not accept the restricted key afterwards | 422 `verify_auth_failed` |
| The forced helper did not return a valid install preflight | 422 `verify_helper_failed` |
| The helper could not load the stage-authority key (`/etc` protection) | 422 `verify_authority_failed` |
| Any other host-side failure | 422 `host_bootstrap_failed` |
| Disabled / unwired, busy (`Retry-After`), stopped | 503 `installs_disabled` / `bootstrap_unavailable`, `bootstraps_busy`, `bootstraps_closed` |
| Whole bootstrap exceeded 150 s | 504 `bootstrap_timeout` (rerun is safe) |

The 422 refusals before `verify_*` leave every destination unchanged. The root script
decides every refusal before its first write. A `verify_*` code means the files were
written but the key did not verify; fix the cause and rerun.

## Flow

```
handler (strict decode, admin actor)
 └─ ProbeHostBootstrapService     (validate, capacity 2, 150 s bound, clears password+key, audit line)
     └─ SSHProbeHostBootstrapper  (provisioning adapter; the only SSH code)
         1. parse deployment key, derive "type base64" (no comment)
         2. verify the helper pin from the artifact dir      → no connection on failure
         3. ONE password login; host key checked against the pin during KEX
         4. session A (login user):  Linux? arch? mkdir -m0700 /tmp/phoenix-bootstrap-<128-bit hex>; receive helper
         5. session B (root, or `sudo -S -- /bin/sh -c …` with the password as the only stdin line):
              check everything → then write atomically → remove the upload directory
         6. on any failure after 4: remove the upload directory as the login user (best effort)
         7. NEW connection with the deployment key as root → forced authorized-dispatch
            → fixed install-preflight must report root=1, authority=1, matching kernel/arch
```

Commands are fixed POSIX scripts passed as `/bin/sh -c '<script>' name 'arg'…`. Every
argument is a hub-built value restricted to `[A-Za-z0-9+/=@._:-]`, so nothing can
close the quoting. None is operator free text. The login shell must be POSIX-compatible
(sh, bash, dash, zsh, ash); csh/fish login shells are unsupported.

## What is written on the host

| Path | Owner / mode | Rule |
|---|---|---|
| `/usr/local/bin/phoenix-probe-admin` | root:root 0755, single link | Replaced only if bytes, owner or mode differ (this is also how the helper is **upgraded**). Created as a private `mktemp` file in the same directory, SHA-256 re-checked as root, then `chown`/`chmod` and `mv -f`. |
| `/etc/phoenix-probe/` | root:root 0755 | Created if absent; an existing one must be root-owned and not group/world writable. |
| `/etc/phoenix-probe/stage-authority.pub` | root:root 0644, single link | 65-byte canonical file. Equal content with a wrong mode is rewritten. **Different content is refused** (`host_authority_conflict`), never replaced: rotation stays a deliberate manual step. |
| `~root/.ssh/` | root, 0700 | Created if absent; an existing one must be root-owned and not group/world writable. |
| `~root/.ssh/authorized_keys` | root, 0600 | Gains exactly one line: `restrict,command="/usr/local/bin/phoenix-probe-admin authorized-dispatch" <type> <base64>`. Existing lines are preserved byte for byte, and a missing final newline is supplied. Atomic replace by rename. If the deployment key already appears on any other line (sshd uses the first match, so an unrestricted copy would defeat the forced command), it is **refused** (`host_authorized_key_conflict`). |

When available, `restorecon` relabels each written file (SELinux). The bootstrap does
**not** change `sshd_config`. Root key login must already be allowed
(`PermitRootLogin prohibit-password` or `forced-commands-only`) with the default
`AuthorizedKeysFile`; otherwise verification reports `verify_auth_failed`. Hardening
`sshd` per [the authorization record](M8_BINARY_STAGE_SSH_AUTHORIZATION.md#operator-trust-and-fixed-policy)
(for example disabling password login afterwards) stays operator-owned.

The probe container privilege model is untouched: root + `--cap-drop ALL
--cap-add NET_RAW`, no-new-privileges, read-only.

## Password and key handling

- Used for exactly one HTTP request. There is no journal, queue, retry, database row
  or WebSocket event, and nothing is persisted on the hub.
- Never logged. The only log line is the service audit line: actor ID, helper
  version/architecture and the fixed outcome code. It has no address, user,
  fingerprint, key or host output.
- Never returned. Responses are fixed views and allowlisted codes; unknown codes fail
  closed with 503.
- On the wire, only through SSH user authentication (`password`, or one echo-off
  `keyboard-interactive` prompt) and, for a non-root user, as the single stdin line of
  `sudo -S`. Never on a command line or in an environment variable. Never written to
  any host file. The root script redirects stdin from `/dev/null` and never reads it.
  Remote stderr (sudo prompts, tool diagnostics) is discarded, and stdout is bounded
  to 4 KiB.
- **Zeroed.** The handler and the service clear the owned password/key byte buffers
  and the `sudo` stdin line once the request settles. Go strings produced by JSON
  decoding and by `x/crypto/ssh`'s string-typed password callbacks cannot be zeroed;
  they become unreachable when the request returns (the same best-effort limit as the
  installer's `private_key`).
- **Frontend.** The probes page's "Prepare host (SSH password)" form clears the
  password and key fields when the request settles, whatever the outcome.

## Verification

| Executed | Result |
|---|---|
| `go test -race -count=1 ./internal/adapters/provisioning -run TestSSHHostBootstrap` (Darwin/arm64: BSD userland, `/bin/sh`) | 8 tests / 16 subtests passed |
| Same, Linux/arm64 container `golang:1.26-bookworm` (dash, GNU coreutils/findutils), non-root uid 1000 | passed |
| `go test -race ./internal/core/services -run TestProbeHostBootstrap` | passed (fake port) |
| `go test -race ./internal/adapters/http/handlers -run TestProbeHostBootstrap` | passed |
| `go test -race ./internal/bootstrap -run TestProbeHostBootstrapAPI` (real router, auth, rate limit, composition, service, adapter → loopback SSH) | passed |
| `go test -tags m8bootstrap -race ./internal/adapters/provisioning -run '^TestM8HostBootstrapOpenSSH$'` against a disposable `debian:bookworm` container: OpenSSH 9.2p1, sudo 1.9.13p3, non-root sudoer with a `/bin/sh` login, production layout, real `phoenix-probe-admin` linux/arm64 | passed: first run installed/installed/added + verified via real forced `authorized-dispatch install-preflight`; rerun unchanged; wrong password, foreign authority and wrong pin refused |

The adapter fixture is a loopback Go SSH server. Password sessions run the real
scripts with the local `/bin/sh` against a private prefix. A PATH shim supplies Linux
`uname` output and a `sudo` that checks the password it reads from stdin. The fixture
covers:
- one password login for every step;
- exact bytes, modes and `authorized_keys` content (including no trailing newline);
- idempotent rerun and mode repair;
- wrong pin (no password offered), wrong password, sudo refusal and architecture
  mismatch;
- unrestricted duplicate key, foreign authority and a group-writable `.ssh` (host tree
  byte-identical after each refusal);
- rejected root key, artifact refusals before any connection, and cancellation;
- the password never on a command line, in a host file or on stdin other than the
  sudo line.

Not covered:
- RHEL/SELinux and `requiretty` hosts;
- csh/fish login shells;
- keyboard-interactive-only sshd;
- amd64 real-host runs (only arm64 was exercised);
- the production `/tmp` on a `noexec` mount (only data is written there, so it should
  not matter, but this is unverified).

No database or schema change.

Run the acceptance gate yourself (it writes root-owned files; use a throwaway host):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -o /tmp/helper ./cmd/phoenix-probe-admin
M8BOOT_ADDR=127.0.0.1:22022 M8BOOT_HOST_FP='SHA256:…' M8BOOT_USER=ops M8BOOT_PASSWORD='…' \
M8BOOT_HELPER=/tmp/helper M8BOOT_VERSION=0.0.0-bootstrap M8BOOT_ARCH=arm64 \
  go test -tags m8bootstrap -count=1 ./internal/adapters/provisioning -run '^TestM8HostBootstrapOpenSSH$' -v
```
