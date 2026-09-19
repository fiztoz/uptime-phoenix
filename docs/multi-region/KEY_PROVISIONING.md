# Prepared snapshot key provisioning

The foundation can now create and load the installation key used to protect
prepared configuration snapshots. This tool does not enable probes, activate
snapshots, rotate keys, or connect to a database. The ordinary application still
boots without a probe key. Runtime wiring and atomic activation remain open.

## Create once

Build the tool from this branch, then run it as the account that will own the key.
Choose a persistent directory outside the checkout and application database.
The parent must already exist, be owned by that account, and have mode `0700`.

```sh
make build-probe-key
mkdir -m 700 /persistent/private-probe-key
./bin/phoenix-probe-key init --file /persistent/private-probe-key/snapshot.key
./bin/phoenix-probe-key check --file /persistent/private-probe-key/snapshot.key
```

Both commands also accept `PROBE_SECRET_KEY_FILE`; `--file` overrides it. This
environment variable is currently consumed by this tool only. It is not yet a
hub/worker startup option. The tool is built explicitly; release images and
archives do not yet package it.

`init` obtains 32 random bytes from Go's cryptographic random source. It writes a
new `0600` staging file in the same directory, syncs and closes it, publishes the
complete file with an atomic hard link that refuses replacement, removes the
staging link, and syncs the directory. Success is reported only after those steps
complete. Use a persistent filesystem supporting hard links and file/directory
sync. Finish provisioning before starting any future snapshot writer.

Existing destinations always cause a nonzero exit, including an empty file,
malformed key, directory, or dangling symlink. Concurrent creators produce one
winner; losers cannot overwrite its key. There is no force or automatic repair
option. The key is never printed. Exit codes are `0` for success/help, `1` for an
operation failure, and `2` for invalid command arguments.

## Load and check

`NewProbeConfigProtectorFromFile` constructs the existing AES-256-GCM adapter from
the file. It accepts exactly 32 **raw bytes**, including zero or newline bytes;
it does not trim, hex-decode, or base64-decode input. The reader checks the opened
descriptor for a regular file, ownership by the current effective user or root,
and mode exactly `0400` or `0600`. The file must also be readable by that process.
Permissions are never repaired by loading. Ownership and permission checks are
implemented for Linux and macOS; other platforms fail closed.

The parent directory must be owned by the current user or root and must not be
writable by group or others. Keep the directory tree, ancestor directories, and
ACLs under trusted operator control. Reading permits relative symlinks that stay
within the opened parent directory, including a projected Secret's
`key -> ..data/key -> timestamp/key` layout. Absolute links and links escaping
that directory are rejected. The final file still needs the restrictive mode and
readable ownership; a default `0644` Secret file is rejected. For a non-root
process, provision an appropriately owned private file instead of relaxing group
permissions. Helm does not install or generate a probe key yet.

Reads are bounded to 33 bytes and reject short or oversized files. FIFO opens are
nonblocking and rejected as non-regular files. Diagnostics include fixed operation
descriptions and filesystem error categories, without paths or key bytes. The
temporary byte buffer is cleared after the adapter copies it; the running process
still holds the cryptographic key schedule needed to decrypt snapshots.

`check` proves only that the file can construct the protector. It cannot establish
entropy, match a key to a database, or establish execution readiness. A different
valid 32-byte key passes `check` but fails snapshot authentication. The future
startup path must authenticate retained snapshots before using this key for new
configuration. Loading never creates a missing key or falls back to a different
key after an error.

## Backup, interruption, and recovery

Back up the exact binary key separately from the encrypted database, with access
controls at least as restrictive as the original. All future processes sharing
one installation's encrypted snapshots must use the same key. Keep the key for
as long as any retained database backup needs it. Verify recovery by restoring
the key and database to an isolated environment and authenticating a retained
snapshot; a successful file-only check is insufficient.

Replacing the key is **not** rotation. Existing ciphertext cannot be recovered
with a newly generated key, and this slice supplies no re-encryption workflow.
If a key is missing or malformed for an installation with retained snapshots,
restore the original from its protected backup. Do not run `init` as a startup
fallback. Loss of every key copy makes the protected snapshots unrecoverable.

A crash before publication can leave a private `.phoenix-probe-key-*` staging
file. The reader never discovers or adopts these files. A crash or I/O failure
after publication can leave the destination present even if the command did not
report success. Preserve it, check it, and establish filesystem durability and
backup before use; never delete it just to make `init` succeed. Only remove
abandoned staging files after establishing that no creator is running.
