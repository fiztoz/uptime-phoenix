# M2 third slice: bounded established-session transport

Keep Gemini 3.8 Flash. This is a bounded transport slice; Codex owns enrollment,
runtime negotiation, edge persistence, hub connector leases, bootstrap and all
existing files outside your identity slice. No commits, pushes, deployments or
new dependencies. Read PROTOCOL.md sections 1–2 and ARCHITECTURE.md session rules.

First finish this small identity correction: the duplicate-key error in
`parseAndValidateManifest` includes the attacker-controlled key text. Use a fixed
redacted error and add a duplicate key containing a sentinel secret to the test.
Then stop editing identity files, update its handoff, and own only:

- `internal/adapters/probe/session.go`
- `internal/adapters/probe/session_test.go`
- `docs/multi-region/M2_SESSION_HANDOFF.md`

## Frozen API

```go
type SessionConfig struct {
    Generation Decimal // positive; negotiation has already completed
    PeerRole string    // "hub" or "probe", for health sender validation
}
type Session struct { /* private state */ }
func NewSession(conn *websocket.Conn, cfg SessionConfig) (*Session, error)
func (s *Session) Run(ctx context.Context, handle func(context.Context, Envelope) error) error
func (s *Session) SendControl(ctx context.Context, frame []byte) error
func (s *Session) SendReplay(ctx context.Context, frame []byte) error
func (s *Session) Close() error
func ReconnectDelay(failures int, healthyFor time.Duration, random float64) time.Duration
```

Exactly one reader and one writer per socket. The caller completes hello/welcome
before constructing Session. Run is single-use; a repeated/concurrent Run returns
a clear error. All incoming/outgoing envelopes match the configured generation;
reject handshake/enrollment frames during established runtime. Reject binary,
oversized and malformed frames. Incoming health payloads must match PeerRole and
pass DecodeHealth. Do not log payloads or return raw peer JSON/parser errors.

Use coder/websocket, compression remains disabled by the accept/dial owner.
SetReadLimit(1 MiB). Writes have a ten-second context deadline. Each read must
receive an application frame within 45 seconds (peer health is every 15 seconds).
Cancellation or read/write failure cancels Run, closes the socket with CloseNow,
and unblocks all pending sends. Close is idempotent and concurrency-safe. Handler
is invoked serially with a bounded ten-second context; it must honor cancellation
and returns errors instead of launching untracked background work.

Maintain separate bounded queues (16 control frames, 16 replay frames). Clone
caller bytes after validation so mutation cannot alter queued content. Replay
accepts only telemetry.batch/gap traffic and enforces the existing frame/batch
size limits. Control must reject replay types. No unbounded goroutine per send.
SendControl/SendReplay return after the writer has actually written the frame,
or on cancellation/failure. Context expiry while queued must prevent a subsequent
write of that item. A canceled write must close the session because partial frame
delivery is uncertain. Never return success merely for enqueueing.

Prioritize control, but after at most eight consecutive control frames write one
waiting replay frame. If a class is empty, progress the other immediately. Test
both control latency under replay load and replay progress under control load.
Do not create synthetic ACKs, storage success, config activation or readiness.

ReconnectDelay is a pure bounded helper: exponential 1,2,4,8,16,30-second base
for failures 1 onward, jitter within 75%–125%, clamped to 1–30 seconds. Reset to
the first base only after at least 30 seconds of healthy operation. Clamp invalid
random values into [0,1], and handle large/negative counters without overflow.

## Verification

Use real local WebSocket pairs. Test concurrency under race, validation failures,
generation mismatch, health role mismatch, single Run, cancellation/close while
queues are full, failure propagation, frame limits, ownership of byte slices,
and fairness. Avoid sleeps or tests taking 45 seconds; use bounded contexts and
internal test-only timing hooks if necessary without exposing unsafe production
timeout overrides. Verify focused tests once to a log, inspect exit code, run
package lint. Report only observed evidence and exact limitations. Codex will
independently run the integrated gates; this alone does not complete M2.
