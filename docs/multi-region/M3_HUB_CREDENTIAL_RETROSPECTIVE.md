# Hub credential rotation retrospective

Baseline: `e23d7e0`. These defects were found in Codex's integration work before
commit. Antigravity was a read-only reviewer and owned no implementation files.

## Immediate reconnect starved a durable receipt

The first compiled process run reached preparation on the edge seven times but
never confirmed it on the hub. The source contained one applied prepare receipt;
the hub prepare stayed pending, and activation stayed blocked. The real TLS test
reproduced the same failure. A test-only dispatcher observer recorded
`context=context canceled commit=context canceled` after the result arrived.
No debugger was installed, so the investigation used source tracing and that
nonsecret callback observer. The observer was removed after the mechanism was
established.

`SendControl` proves a wire write. It does not prove the receiver's asynchronous
work lane committed the result. The old source returned an error immediately
after sending a successful credential result. Session shutdown canceled the hub
lane while its database transaction was starting. Reconnecting and returning the
same source receipt reproduced the same cancellation indefinitely. Isolated
storage tests and a client that only reads frames cannot reveal this lifecycle
failure; the real two-store path did.

The source now quiesces further effects after the result write, retaining independent
health handling while awaiting hub closure. The hub requests reconnect only after
its durable receipt commits. A 12-second source timer bounds an uncooperative peer;
the original admitted credential deadline remains authoritative. A close/timeout
is never treated as success. Retry still uses immutable requests and source receipts.
The regression additionally proves both-store restart after actual source activation
with the reply deliberately lost. Another test leaves the peer open, sends a second
effect, verifies it is not applied, and observes bounded source closure.

## Cleanup removed a live rotation dependency

The initial shared ledger cleanup deleted every confirmed request after its
retention deadline. A confirmed prepare may still be needed by an unresolved
activation: candidate selection and result completion authenticate both original
requests. `UnresolvedRetentionDependency` reproduced this by aging the prepare
retention field, issuing an unrelated ACK (which runs cleanup), and then selecting
the candidate. It failed with `not found` because the prepare body was gone.

Cleanup now excludes both command IDs of every retained rotation. Terminal
rotation cleanup releases the dependencies first; an unresolved operation remains
bounded but cannot be silently evicted. The focused contracts exercise the rule
on both actual engines. Receipt age is not proof that its dependents are finished.

## Review and test corrections

Antigravity's initial audit and first corrective-feedback runs returned service-error status despite
producing advisory text. Neither is counted as a completed independent audit.
Its claimed SQLite cross-column CHECK syntax blocker was disproved by the actual
modernc SQLite migration and contracts. Its PreparedAt concern conflated the hub
candidate preparation timestamp with the separate source preparation receipt;
these deliberately record different events. It also missed the retention
relationship above. Codex sent those verified corrections and coding lessons;
the returned feedback explicitly withdrew both false findings and explained the
missing cleanup dependency. No project rules or source were delegated to it. A final short lifecycle-feedback
run completed successfully (conversation `4cc9b112-e986-4997-8add-2f963c81ef7d`)
and explicitly acknowledged the commit-before-close, cleanup dependency and
timestamp/engine-verification lessons. That acknowledgement is not an independent
code audit or test result.

The first new test fixture used noncanonical Base64 and was correctly rejected
before issuance; it now generates the fixture token using the actual encoding.
A new TLS test initially assumed a pointer for the existing value-typed retry
timestamp; compiling it exposed that mismatch. Lint subsequently identified a
new internal status spelling and the deprecated Bun `In` helper; the code now
uses `canceled` and the installed `List` API. These failed checks are preserved
in the evidence ledger rather than described as uninterrupted success.

## Prevention

Trace durable effects through the production transport lifecycle, including who
cancels each context and who joins each worker. Distinguish persisted intent,
wire transmission, peer authentication and committed application. Keep cleanup
aware of unresolved dependencies. Verify engine claims with executable evidence,
and read concrete field/signature contracts before authoring callers or tests.
Retain failure logs and report provisional reviews as provisional.

Validation and remaining limits: see
[acceptance](M3_HUB_CREDENTIAL_ACCEPTANCE.md) and the accompanying evidence manifest.
