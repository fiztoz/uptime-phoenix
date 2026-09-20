# Identity slice integration review

The subprocess lock test is useful, but the current implementation does not yet
meet its file-safety contract. Keep ownership of the same five files and correct
these before moving on. Do not claim this closes M2.

1. `acquireDirLock` calls `os.OpenFile` before checking the file. `f.Stat` follows
   symlinks and can never detect that the path was a symlink. A symlink to a regular
   file is accepted, its ownership/mode is not validated, and a FIFO may block in
   open before cancellation is considered. Add a failing lock-symlink test first,
   then use no-follow, nonblocking descriptor-relative open under a held directory
   handle. Verify regular type, owner and exact 0600 mode on that descriptor.
   Keep the lock inode stable and do not unlink it. Test a permissive existing
   lock file and FIFO with a bounded subprocess timeout as well.

2. Manifest and TLS reads use `Lstat` followed by unbounded `os.ReadFile`. Open
   under `os.Root` with no-follow/nonblocking flags, validate the opened descriptor,
   and bound reads (for example 4 KiB manifest and 64 KiB PEM). A check on a path
   followed by a second open is not the same as checking the file actually read.
   Reject duplicate JSON keys and trailing data; do not echo JSON parse errors
   that might include file contents. Add oversize and duplicate-field tests.

3. `filepath.Abs` is not canonicalization through parent symlinks. Resolve and
   validate the actual directory, keep a directory handle through file operations,
   and consistently require 0700 for the directory and 0600 for created files.
   Avoid check-then-use publication through absolute paths; the existing auth
   key helper demonstrates `os.Root`, no-replace link publication and directory
   sync. Ensure partial initialization never replaces prior valid material.

4. `uuid.New` may panic when entropy fails. Use `uuid.NewRandom` and handle the
   error explicitly. Keep filesystem/helper errors bounded and do not expose
   user-supplied path strings or PEM contents in diagnostics.

Run focused identity race tests after these changes and inspect the exit code.
Run the package lint using the installed absolute path. Preserve all other files.
Update the handoff with the failures you reproduced and corrected, and distinguish
actual non-Unix compilation from merely having a build-tagged fallback file.
