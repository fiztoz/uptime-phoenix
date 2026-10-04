# M3 bounded shutdown and pressure diagnostics

> Historical checkpoint: implementation limits and remaining work below describe
> that checkpoint. Use [current status](IMPLEMENTATION_STATUS.md) and
> [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md) for today's scope.

Date: 2026-09-21. Baseline `98f136a`. This shutdown checkpoint is accepted with
the evidence below. It does not complete M3. Codex owns code and verification;
Antigravity supplied advisory review only. No push or deployment.

## Lifecycle

Termination stops new HTTP requests, runtime sessions, management mutations,
scheduled checks and provider claims. Already-admitted management callbacks join
under their existing ten-second handler deadline. A flag alone is insufficient.
An independent producer context permits checks and provider attempts up to ten
seconds to finish and commit results; expiration cancels supported adapters and
joins them. Watchdog and retention work stop independently. HTTP tracking includes
hijacked enrollment requests, which `http.Server.Shutdown` does not join itself.

After producer and management joins, the runtime captures a fixed durable stream
prefix and gives the sole existing replay pump five seconds to receive its ACK.
The observer cannot send, prune or advance a cursor. A changed identity, moving
producer prefix, cursor rollback, failed read or deadline fails the drain. A
matching committed ACK completes it. Every unacknowledged byte remains durable.
Session handlers close and join before storage and the exclusive directory lock
are released. HTTP shutdown has a separate five-second ceiling. Normal supported
I/O fits below the deployment's default 30-second termination grace.

Health uses the existing coherent diagnostics: pressure begins at 80% of the
configured telemetry byte budget, including conservative row/gap overhead.
Declared gaps remain visible until their committed ACK. Shutdown stops readiness;
hub connectivity alone still does not determine ordinary autonomous readiness.

## SMTP cancellation correction

The concrete SMTP provider originally ignored `ctx`. Its mail library waited for
the server greeting before installing an I/O deadline and could reconnect
implicitly. A silent server could therefore prevent the producer join forever.
SMTP now owns a context-aware socket, installs its deadline before greeting/TLS,
and closes on cancellation. A single ten-second budget covers network operations.
The existing mail library still composes MIME and envelope addresses. Mandatory
STARTTLS, implicit TLS on port 465 and the existing AUTH selection are preserved.
Durable delivery owns retries; the transport does not retry implicitly.

No shutdown protocol makes provider delivery exactly once. Cancellation after
external acceptance but before the local outcome commit remains an uncertain
attempt that can be retried after lease expiry. No successful receipt is invented.

## Verification

The focused race cases passed, followed by all four affected package suites
(`cmd/probe`, probe transport, scheduler and core services). The final notifier
race suite also passed, including greeting cancellation, pre-canceled context,
DATA-receipt cancellation, mandatory-TLS refusal and LOGIN compatibility.

The first compiled replay/shutdown run passed 25 stages. A healthy stop completed
in 0.004 seconds with cursor 13/13; offline stops completed in 5.043 and 5.033
seconds, preserving four and one exact queued events respectively. That run
preceded the SMTP correction. The fresh compiled rerun including SMTP also passed
25 stages: a healthy 0.004-second stop and offline 5.015/5.032-second stops retained
four/one exact events. SMTP-specific network failures are proven by the notifier
tests; the process harness uses a local webhook. Do not present webhook delivery
as SMTP protocol proof.

The complete race suite passed 3,708 named cases in 22 packages, with zero failures
and two existing optional skips. All 315 MariaDB-named cases passed, including 302
audited live-engine cases, with no engine skips. Final lint caught two redundant
SMTP interface declarations; only those two identical inferred-type substitutions
followed the full race run. Final CGO-free build, complete notifier race suite and
zero-issue lint passed afterward. All other 13 Go source hashes match the full
race run. The original SMTP hash is reconstructible by reversing the two edits.

Frontend type checking had zero errors/warnings, all 251 unit tests passed, build
and lint passed, and all 12 Chromium journeys passed. Helm lint, all gate template
variants and topology assertions passed. `govulncheck ./...`, `go vet ./internal/...`,
formatting and whitespace checks passed. See [evidence](M3_SHUTDOWN_EVIDENCE.json).

```text
Commands executed: CGO_ENABLED=0 go build ./...; go test -race -count=1 -timeout=20m -json ./...; final notifier race suite and golangci-lint run; go vet; frontend check/test/build/lint/test:e2e; Helm checks; govulncheck; compiled --verify-replay --verify-shutdown.
Engines and named tests exercised: real SQLite, disposable MariaDB, TLS replay, HTTP check/provider, SMTP greeting/DATA cancellation, complete repository suite and Chromium.
Passed / failed / skipped: 3,708 full-suite passes, zero race failures, two optional skips; 25 process stages; final lint zero issues after the recorded style correction.
Acceptance criteria still unverified: metadata/history bounds and cleanup, complete 900-second M3 partition and final requirement audit.
```

Remaining whole-M3 requirements include source metadata/history storage bounds,
the actual 900-second partition with DOWN/UP, restart, current state ahead of
backlog and original-incident ACK, followed by the full requirement audit and
verification of any subsequent changes. The [final M3 acceptance](M3_COMPLETION_ACCEPTANCE.md)
records the completed storage and partition gates.
