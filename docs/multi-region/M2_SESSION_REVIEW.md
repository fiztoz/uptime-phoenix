# Session integration review while tests are running

Keep ownership of the same three session files. Correct and test these before
handoff; do not edit core/edge persistence or the listener owned by Codex.

1. `Run` assigns `s.cancelRun` while concurrent `Close` reads it without a lock.
   `sync.Once` protects Close callers, not an unrelated Run assignment. A race
   can also close the session before the cancellation hook is installed. Use a
   mutex or a cancellation design initialized by NewSession, and test concurrent
   Run/Close and Close-before-Run repeatedly under `-race`.

2. The writer derives its write context only from the Run context. It checks an
   item's context before starting but ignores cancellation during the write.
   Send then returns the caller's cancellation while the actual write may still
   continue. Derive the write operation from BOTH session and caller cancellation
   and the ten-second deadline (context.AfterFunc can connect cancellation without
   leaving a goroutine per queued item). An interrupted write closes the session.
   Add a real backpressured WebSocket test that cancels the in-flight sender and
   proves the socket and sibling sends terminate promptly.

3. Incoming read errors and outgoing decoder errors are currently returned raw.
   WebSocket CloseError contains a peer-controlled reason; JSON errors can include
   field values. Redact transport/parser errors while preserving context canceled
   and deadline sentinels where useful. Test a peer close reason and malformed
   decimal field containing a sentinel secret.

4. nextItem prioritizes nonempty queues before checking session cancellation.
   Ensure cancellation is checked before any new write even when both queues are
   continuously populated. The selected item's context must also be checked at
   the actual write boundary. Keep the eight-control fairness bound.

The initial integrated compile raced your in-progress test file and observed an
unused net import; Codex will rerun its tests after your handoff rather than edit
your files. Report the exact passing evidence only after these changes.
