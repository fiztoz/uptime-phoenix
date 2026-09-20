# M2 Runtime Identity & Directory Ownership Slice Handoff (Review Corrections)

## 1. Scope & Ownership

This document records the corrective implementation and verification of the second Milestone 2 slice as specified in `docs/multi-region/M2_IDENTITY_WORK_CONTRACT.md`, addressing all four findings from `docs/multi-region/M2_IDENTITY_REVIEW.md`.

Owned and modified files:
- `internal/adapters/probe/runtime_identity.go`
- `internal/adapters/probe/runtime_identity_unix.go`
- `internal/adapters/probe/runtime_identity_other.go`
- `internal/adapters/probe/runtime_identity_test.go`
- `docs/multi-region/M2_IDENTITY_HANDOFF.md`

All existing modified files, shared docs, core contracts, edge SQLite implementation, and migrations remain owned by Codex. No commits, pushes, deployments, or new dependencies were introduced. This slice alone does NOT complete M2.

---

## 2. Review Findings & Applied Resolutions

### Finding 1: Lock Symlink & FIFO Blocking
- **Problem**: `acquireDirLock` previously opened the lock path with `os.OpenFile` and called `f.Stat()`. `f.Stat` follows symlinks and could never detect that `probe.lock` was a symlink. A symlink to a regular file was accepted, ownership and mode were unvalidated on the symlink target, and a FIFO would block in `open` before cancellation could be evaluated.
- **Reproduction**: Added `lock is symlink`, `lock is permissive`, and `lock is fifo` tests. Before the fix:
  - `lock is symlink` passed without error (exit code 1 in test suite): `expected lock symlink rejection, got <nil>`.
  - `lock is permissive` (mode 0644) passed without error: `expected lock permissive rejection, got <nil>`.
- **Resolution**:
  - `acquireDirLock(dirFile *os.File)` opens the descriptor relative to the held directory handle `dirFile` using `unix.Openat(dirFd, lockFileName, unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)`.
  - If `ENOENT`, creates with `O_CREAT|O_EXCL` and explicitly sets mode `0600` via `unix.Fchmod`.
  - Inspects descriptor via `unix.Fstat`: rejects non-regular files (`st.Mode & S_IFMT != S_IFREG`, immediately rejecting FIFOs without blocking), rejects permissive modes (`st.Mode & 0777 != 0600`), and verifies process ownership (`st.Uid == os.Geteuid()`).
  - Kernel flock (`unix.Flock`) is acquired non-blocking (`LOCK_EX|LOCK_NB`).
  - The lock inode is never unlinked on release, preserving inode stability across processes.

### Finding 2: Bounded Descriptor Reads & Strict Manifest Validation
- **Problem**: Manifest and TLS reads used `Lstat` followed by unbounded `os.ReadFile` via path strings (check-then-use TOCTOU race). Standard `json.Unmarshal` silently accepted duplicate JSON keys and ignored trailing data. Raw JSON parse errors could echo file contents.
- **Reproduction**: Added `duplicate keys`, `trailing data`, `oversize manifest`, and `oversize tls.pem` tests. Before the fix:
  - `duplicate keys`: silently accepted, overwriting earlier keys.
  - `trailing data`: failed with raw parser error echoing content: `invalid character '{' after top-level value`.
  - `oversize manifest` and `oversize tls.pem`: read unbounded files without rejection.
- **Resolution**:
  - Implemented `readDescriptorBounded(root *os.Root, filename string, maxBytes int64)`:
    - Opens under `os.Root` with `os.O_RDONLY | probeNonblock | probeNoFollow`.
    - Validates descriptor: `info.Mode().IsRegular()`, `info.Mode().Perm() == 0600`, and `probeTrustedOwner(info)`.
    - Bounds reads: max 4096 bytes (4 KiB) for `identity.json`, max 65536 bytes (64 KiB) for `tls.pem`.
    - Rejects if `info.Size() > maxBytes` or if `io.LimitReader` yields more than `maxBytes`.
  - Implemented `parseAndValidateManifest`:
    - Uses `json.NewDecoder` token scanner.
    - Rejects duplicate keys via seen-key map with fixed redacted error: `duplicate key in identity manifest` (attacker-controlled key text is never echoed).
    - Rejects trailing data/tokens after closing `}`.
    - Redacts JSON parse errors to fixed messages (e.g. `parse identity manifest failed: invalid JSON`) without echoing file content.

### Finding 3: Canonicalization Through Parent Symlinks & Atomic Publication
- **Problem**: `filepath.Abs` did not resolve parent symlinks (e.g. `/var` -> `/private/var` on macOS or symlinked parent directories). Files were published via absolute paths with check-then-use races.
- **Reproduction**: Added `parent is symlink canonicalization` test. Before the fix:
  - `expected canonical DataDir "/private/.../real-parent/probe-data", got "/var/.../sym-parent/probe-data"`.
- **Resolution**:
  - `prepareDataDir` canonicalizes paths using `filepath.EvalSymlinks(cleanDir)` + `filepath.Abs`.
  - Enforces that neither the input path nor canonical path is a symlink.
  - Opens `os.OpenRoot(canonicalDir)` and holds `*os.Root` across the identity lifecycle.
  - Validates directory descriptor: `0700` permissions and current user ownership.
  - Implemented `publishFileNoReplace(root *os.Root, filename string, data []byte)`:
    - Staging file created under `root` with `os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600`.
    - File fsync + atomic `root.Link(staging, filename)` (fails if destination exists) + staging remove.
    - Directory sync via `root.Open(".")` + `dir.Sync()`.
  - Partial initialization recovery reuses valid existing `tls.pem` without regenerating or replacing it.

### Finding 4: Entropy Failure & Error Redaction
- **Problem**: `uuid.New` could panic on entropy exhaustion. Errors could expose user-supplied paths or PEM content.
- **Resolution**:
  - Replaced `uuid.New()` with `uuid.NewRandom()` and explicit error handling.
  - Implemented `probeIOError`: wraps standard `io/fs` sentinels (`fs.ErrNotExist`, `fs.ErrExist`, `fs.ErrPermission`, `fs.ErrInvalid`) without returning user paths or PEM content in diagnostics.

---

## 3. Observed Evidence: Failing Before vs Passing After

### A. Failing Reproduction Evidence (Before Corrections)

Command:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -v -count=1 ./internal/adapters/probe -run "^TestRuntimeIdentity"
```

Output:
```text
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/duplicate_keys
    runtime_identity_test.go:346: expected duplicate key error, got certificate fingerprint mismatch between identity.json and tls.pem
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/trailing_data
    runtime_identity_test.go:358: expected trailing data error, got parse identity manifest: invalid character '{' after top-level value
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/oversize_manifest
    runtime_identity_test.go:370: expected oversize manifest error, got certificate fingerprint mismatch between identity.json and tls.pem
--- FAIL: TestRuntimeIdentity_CorruptManifestRejection (0.15s)

=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/oversize_tls.pem
    runtime_identity_test.go:478: expected oversize tls error, got <nil>
--- FAIL: TestRuntimeIdentity_CorruptOrMissingTLSRejection (0.08s)

=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/parent_is_symlink_canonicalization
    runtime_identity_test.go:513: expected canonical DataDir "/private/.../real-parent/probe-data", got "/var/.../sym-parent/probe-data"
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/lock_is_symlink
    runtime_identity_test.go:593: expected lock symlink rejection, got <nil>
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/lock_is_permissive
    runtime_identity_test.go:608: expected lock permissive rejection, got <nil>
--- FAIL: TestRuntimeIdentity_SymlinkAndPermissionRejection (0.10s)

FAIL
FAIL	github.com/fiztoz/uptime-phoenix/internal/adapters/probe	1.078s
EXIT_CODE: 1
```

### B. Passing Evidence (After Corrections)

Command:
```bash
rtk proxy env GOTOOLCHAIN=go1.26.6 go test -v -race -count=1 ./internal/adapters/probe -run "^TestRuntimeIdentity"
```

Output:
```text
=== RUN   TestRuntimeIdentity_InitializeAndOpen
--- PASS: TestRuntimeIdentity_InitializeAndOpen (0.02s)
=== RUN   TestRuntimeIdentity_IdempotentInitialize
--- PASS: TestRuntimeIdentity_IdempotentInitialize (0.02s)
=== RUN   TestRuntimeIdentity_UninitializedOpenFails
--- PASS: TestRuntimeIdentity_UninitializedOpenFails (0.00s)
=== RUN   TestRuntimeIdentity_PartialInitializationRecovery
--- PASS: TestRuntimeIdentity_PartialInitializationRecovery (0.01s)
=== RUN   TestRuntimeIdentity_CorruptManifestRejection
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/corrupt_json
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/unsupported_version
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/nil_uuid
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/uppercase_uuid
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/invalid_fingerprint_length
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/fingerprint_mismatch
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/duplicate_keys
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/trailing_data
=== RUN   TestRuntimeIdentity_CorruptManifestRejection/oversize_manifest
--- PASS: TestRuntimeIdentity_CorruptManifestRejection (0.17s)
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/missing_tls.pem
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/corrupt_tls.pem
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/expired_certificate
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/not_yet_valid_certificate
=== RUN   TestRuntimeIdentity_CorruptOrMissingTLSRejection/oversize_tls.pem
--- PASS: TestRuntimeIdentity_CorruptOrMissingTLSRejection (0.10s)
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/parent_is_symlink_canonicalization
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/directory_is_symlink
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/directory_permissive_permissions
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/manifest_is_symlink
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/tls.pem_is_symlink
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/lock_is_symlink
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/lock_is_permissive
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/lock_is_fifo
=== RUN   TestRuntimeIdentity_SymlinkAndPermissionRejection/permissive_file_permissions
--- PASS: TestRuntimeIdentity_SymlinkAndPermissionRejection (0.14s)
=== RUN   TestRuntimeIdentity_ContextCancellation
--- PASS: TestRuntimeIdentity_ContextCancellation (0.00s)
=== RUN   TestRuntimeIdentity_SecretsNotExposedInErrors
--- PASS: TestRuntimeIdentity_SecretsNotExposedInErrors (0.02s)
=== RUN   TestRuntimeIdentity_SubprocessLockContentionAndRelease
--- PASS: TestRuntimeIdentity_SubprocessLockContentionAndRelease (0.06s)
PASS
ok  	github.com/fiztoz/uptime-phoenix/internal/adapters/probe	2.228s
EXIT_CODE: 0
```

---

## 4. Platform Compilation & Lint Verification

### Non-Unix Cross-Compilation
Command:
```bash
GOOS=windows rtk proxy env GOTOOLCHAIN=go1.26.6 go build ./internal/adapters/probe
```
Result: Exited with code `0` (clean compilation with `runtime_identity_other.go`).

### Linter
Command:
```bash
GOTOOLCHAIN=go1.26.6 /Users/fizto/go/bin/golangci-lint run ./internal/adapters/probe/...
```
Result:
```text
0 issues.
EXIT_CODE: 0
```

### Go Vet & Formatting
- `go vet ./internal/adapters/probe/...`: exited with code `0` (zero warnings).
- `gofmt -l internal/adapters/probe/runtime_identity*.go`: exited with code `0` (empty output, clean formatting).

---

## 5. Scope & Subsequent M2 Dependencies

Completed in this corrected slice:
- Strict descriptor-relative non-blocking no-follow locking under held directory handle.
- Rejection of symlink lock files, non-regular lock files (FIFOs), and permissive lock files (0644).
- Safe bounded descriptor reads (4 KiB manifest, 64 KiB PEM) under `os.Root`.
- Strict JSON manifest parsing with duplicate key and trailing data rejection.
- Path canonicalization through parent symlinks and held `*os.Root` directory handles.
- Atomic no-replace publication with fsync durability.
- Safe entropy generation (`uuid.NewRandom`) and complete diagnostic error redaction.

Excluded (remaining for subsequent M2 slices):
- Edge SQLite migrations and encrypted configuration store.
- Single-use enrollment token generation and exchange.
- Pinned WebSocket connection establishment and session supervisor.
- Autonomous probe execution loop and local telemetry queue.
