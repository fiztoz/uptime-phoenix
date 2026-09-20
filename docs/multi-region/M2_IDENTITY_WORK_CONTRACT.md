# M2 second slice: stable local identity and exclusive directory ownership

This assignment starts only after the pinned-client review is addressed and its
handoff is written. Keep Gemini 3.8 Flash. No DeepCoder, commits, pushes, deployments
or new dependencies. Read ARCHITECTURE.md section on persistent probe identity and
PROTOCOL.md enrollment. This is a dependency of M2, not a milestone completion claim.

## Ownership

Antigravity owns these new files only for this slice:

- `internal/adapters/probe/runtime_identity.go`
- `internal/adapters/probe/runtime_identity_unix.go`
- `internal/adapters/probe/runtime_identity_other.go`
- `internal/adapters/probe/runtime_identity_test.go`
- `docs/multi-region/M2_IDENTITY_HANDOFF.md`

Codex retains core contracts, edge SQLite implementation, enrollment/session
integration, cmd/probe/bootstrap, all M1 files, migrations and shared documents.
Do not edit the pinned-client files further after the first handoff without telling
Codex, who will independently verify them.

## Frozen API (package probe)

```go
type RuntimeIdentity struct {
    ProbeID string
    StreamID string
    Fingerprint string // lowercase SHA-256 leaf DER
    DataDir string     // absolute canonical directory
    Certificate tls.Certificate
    // private directory-lock handle
}
func InitializeRuntimeIdentity(ctx context.Context, dataDir string) (*RuntimeIdentity, error)
func OpenRuntimeIdentity(ctx context.Context, dataDir string) (*RuntimeIdentity, error)
func (identity *RuntimeIdentity) Close() error
```

Initialize is an explicit operator action. Open is normal startup and must fail on
an uninitialized directory. Both hold an exclusive nonblocking OS file lock until
Close. A second process sharing the directory must fail with a bounded clear error;
a crashed process automatically releases the OS lock. Never unlink the lock file
while holding/releasing it (that permits different lock inodes).

Use the existing x/sys/unix dependency for flock on Unix; provide a compilable
non-Unix implementation returning an explicit unsupported-platform error. No CGO.
Use build tags properly. No network listener or HTTP server belongs in this slice.

The directory is private (0700) and owned by the process user. Reject untrusted
writable directories, symlink identity/key/lock files, and nonregular files. Open
files under a directory handle where feasible; do not overwrite existing files
through a symlink. Return bounded errors without PEM/private-key bytes or secret
contents. Do not add logging/printing of keys or tokens.

Persist one atomic 0600 `tls.pem` containing an ECDSA P-256 private key and its
self-signed server certificate (TLS 1.3 compatible, digital signature, server auth).
The explicit fingerprint is the identity; no CA or hostname validation is required
by the pinned transport. Certificate validity should allow small initial clock skew
and last one year. A normal startup must not regenerate, renew or replace material.
Reject expired/not-yet-valid certificates with a clear operator diagnostic.

Persist a versioned 0600 `identity.json` containing canonical nonzero lowercase
UUID probe_id and stream_id and certificate fingerprint. Use explicit DTO fields.
Publish files via private staging + fsync + no-replace publication + directory fsync.
Write the identity manifest last. An explicit Initialize retry after a crash may
finish a partial initialization using an already valid TLS file, but must never
replace valid existing TLS or an existing manifest. If a manifest exists, Initialize
loads and validates it like Open. Missing/mismatched/corrupt material fails closed.

Stable identities survive Close/Open and full subprocess restart. Changing or
copying these files to another directory is not a supported reset; duplicate
identity across different machines will be handled by the session layer later.
Do not claim this local lock solves copied identity.

Enrollment tokens, encrypted accepted configuration, runtime credential hashes,
stream sequence and delivery queues belong to the dedicated edge SQLite adapter
owned by Codex. Do not invent their persistence in this file. The composition root
will separately provision its protected configuration key while this lock is held.

## Evidence

Use temp private directories and real subprocesses for lock contention/release.
Test stable IDs/fingerprint/key across reopen, missing/uninitialized directory,
corrupt/missing/mismatched manifest/TLS, invalid UUIDs, symlink and permission
rejection, idempotent initialization, partial initialization recovery, cancellation,
and key material not appearing in errors. Run focused race tests once to a log,
inspect the exit code, format and lint your package. The installed linter is
`/Users/fizto/go/bin/golangci-lint`; pin `GOTOOLCHAIN=go1.26.6` and prefix shell
commands `rtk proxy`. Report exact commands and distinguish platform-tested behavior
from compilation-only checks. Leave the code uncommitted for integration.
