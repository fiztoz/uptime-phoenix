# Bounded shutdown retrospective

Date: 2026-09-21. Codex owns changes and validation; Antigravity owns no files.
Final checkpoint evidence is tracked in [acceptance](M3_SHUTDOWN_ACCEPTANCE.md).

## Summary and root cause

The edge canceled producers and transport through one context. Durable events
survived, but no flush opportunity remained for final committed results. The new
lifecycle separates producer completion from transport teardown, joins every
source of new events, and observes a fixed durable ACK prefix under a deadline.
Joining also covers already-running management callbacks and hijacked enrollment.

The final concrete-provider audit found a second mechanism: `SMTPSender.Send`
ignored its context. In mail.v2 v2.3.1, `Dial` calls `smtp.NewClient` before setting
the socket deadline; that call waits for the server greeting. The dial timeout
does not bound this established-socket read. Its sender can also reconnect
recursively. Thus an interface taking context and a nominal timeout were not proof
of cancellation. This existing provider defect was found by Codex's source trace,
not by Antigravity's audit.

## Reproduction and fix

`TestSMTPSenderCancellationBeforeGreeting` accepted a local TCP connection without
sending a greeting, canceled the caller and observed the send still blocked after
300 ms. The test then closed its own peer to release the stuck call.
`TestSMTPSenderCanceledContextDoesNotConnect` showed a pre-canceled caller still
delivered to the fake SMTP server and returned nil. Both failed before the fix;
the complete notifier race suite passes afterward.

The adapter retains MIME/envelope composition but owns a context-aware socket and
an absolute network deadline installed before greeting/TLS. Cancellation closes
the socket, including while waiting for DATA acceptance. The transport no longer
retries independently of durable delivery. Existing composition/provider tests,
mandatory TLS refusal and LOGIN compatibility continue to pass.

The full race suite passed 3,708 named cases, then final lint rejected two redundant
interface type declarations in the SMTP change (ST1023). Replacing them with the
identical inferred interface types is the only source change after that suite.
The original SMTP content hash is reproducible by reversing those two edits;
all other source hashes match. Final build, notifier race tests and zero-issue
lint passed afterward. Preserve this distinction in evidence instead of claiming the
first gate had no failure or repeating unrelated tests for a type-style edit.

## Antigravity review disposition

The first advisory review correctly identified the coupled shutdown context, but
incorrectly said `FinishDelivery` emits no telemetry. The concrete store allocates
a source sequence and appends `delivery.result` in the outcome transaction. Codex
also corrected its flag-only mutation barrier and its claim that runtime Close
sends a graceful close frame; the actual call is `CloseNow`. Antigravity explicitly
acknowledged those lessons without editing files.

A separate storage audit returned an empty response after a denied command. It
provides no finding or acceptance evidence and no permissions were changed.

The subsequent code-only audit proposed three concerns, all later retracted:
it speculated that canceling one SQLite operation poisons another without tracing
the driver's transaction lifecycle; it hypothesized a delegated delivery task
even though `process` and its outcome commit are synchronous and the owning worker
is joined; and it restated the explicitly documented provider-acceptance/local-
commit ambiguity as an unbounded-context issue. The producer context is canceled
at the grace deadline. Codex supplied those exact paths and the SMTP reproduction;
Antigravity explicitly retracted all three claims and acknowledged the concrete
provider-boundary lesson. These claims must not replace the actual reproduction.

## Prevention

Trace concrete adapters and database effects through their complete call paths.
Distinguish canceled work from completed work, source outcomes from local
observations, and static concerns from reproductions. Test a silent peer at each
protocol boundary, including before the first greeting. Preserve provider
uncertainty honestly. Count a flush only from a persisted matching ACK, and keep
storage open until every callback that can reach it has joined.
