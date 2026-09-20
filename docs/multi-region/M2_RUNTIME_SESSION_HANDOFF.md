# Integrator runtime-session handoff

Gemini reached its quota before writing this slice. Codex implemented it under
the frozen API in `M2_RUNTIME_SESSION_WORK_CONTRACT.md`. Antigravity's next role is
an independent read-only review after its quota resets, not duplicate implementation.

`runtime_session.go` validates authenticated local binding, sends hello and checks
welcome within ten seconds, then persists a strictly increasing generation before
starting health or configuration handling. Equal generations are rejected in the
edge transaction itself. A rejected newcomer leaves the incumbent alive; a higher
generation closes it. Close stops admissions, cancels/joins handshakes and sessions,
and lets the composition root close SQLite after all callbacks exit.

One Session reader/writer carries role-checked health every 15 seconds and bounded
config transfer frames. Configuration uses original document bytes, full validators,
protected storage and a generation-fenced atomic activation. A failed activation
produces a redacted rejected receipt; applied is sent only after commit. Welcome
does not change telemetry cursors. Replay/current-state/commands remain M3.

`TestEdgeRuntimeFencingConfigReceiptAndShutdown` runs actual pinned TLS with a real
edge SQLite store. It checks duplicate/foreign-stream/ahead-cursor rejection,
incumbent survival, replacement, durable fence before health, injected activation
failure without an applied receipt, exact decrypted bytes, and repeated shutdown.
The first fixture omitted the mandatory `snapshot.v1` transfer requirement and was
correctly rejected; after fixing that fixture, the race test passed:

`GOTOOLCHAIN=go1.26.6 go test -race -count=1 ./internal/adapters/probe -run '^TestEdgeRuntime'`

Independent output: `/private/tmp/phoenix-m2-runtime-session-race2.log`.
The entry point compiles and CLI initialization/retention tests pass. Full process
acceptance and hub connector integration remain pending; this is not M2 completion.
