# Edge authenticated runtime session work contract

**Integrator takeover, 2026-09-20 12:30 local:** Gemini reached its quota before
creating any files. Codex now owns implementation of this slice during the wait.
On resumption Antigravity must review the landed implementation read-only, writing
`M2_RUNTIME_SESSION_REVIEW.md`; do not overwrite runtime or command files. The API
and requirements below remain the implementation contract.

Keep Gemini 3.8 Flash High. Own ONLY `internal/adapters/probe/runtime_session.go`,
`runtime_session_test.go`, and `docs/multi-region/M2_RUNTIME_SESSION_HANDOFF.md`.
Other files belong to Codex. No commits/pushes. This slice is not M2 completion.
Use existing Session/handshake/config transfer codecs; do not reimplement them.

Frozen adapter API (package probe):

```go
type EdgeRuntimeConfig struct { AgentVersion string; Capabilities []string }
// state returns one coherent durable identity and the first retained sequence
// (zero when no retained telemetry); caller provides trusted local storage.
type EdgeRuntimeState func(context.Context) (domain.EdgeIdentity, int64, error)
func NewEdgeRuntime(state EdgeRuntimeState, identity ports.EdgeIdentityRepository,
  configs *services.EdgeConfigService, cfg EdgeRuntimeConfig,
  health func(context.Context) (Health,error)) (*EdgeRuntime,error)
func (*EdgeRuntime) Handle(context.Context,*websocket.Conn,domain.EdgeEnrollment) error
func (*EdgeRuntime) Close() error
```

Validate dependencies/config/capabilities, clone capability inventory. Handle is
called ONLY after header authentication by existing EdgeHTTPHandler. Verify binding
matches durable local hub/probe; stream exclusively from local state. Send hello
gen0 with exact durable counters, protocol1, capabilities and empty resource_bindings.
Read welcome under ONE 10-second handshake deadline, text only, limit1MiB. Use
ValidateHandshake against local IDs and the returned positive welcome generation;
require generation STRICTLY GREATER than persisted accepted generation (also re-read
under a mutex at commit to reject concurrent duplicates). Persist through identity
AcceptConnectionGeneration before any config/health session activity. Never apply
welcome cursor or delete telemetry (M3). A higher generation replaces/closes the old
session; an equal/stale/invalid newcomer must NOT close the valid incumbent. Close
is idempotent, permanently stops new Handle, cancels/joins handlers as practicable
without deadlock and closes the active socket. Do not hold mutex over network waits.

Established session uses NewSession(PeerRole:hub). Send validated role:probe health
every15sec starting immediately, independent of configuration-transfer frames.
Bound the health callback and sends; Session.Run is the one reader/writer. No nested
reader or direct runtime conn.Write calls. Handle health accepts hub-role only.

Support config.begin/chunk/commit using ConfigTransfer and CommitDocument, scoped
to trusted current generation/IDs/capabilities and a 60-second (codec-defined)
transfer deadline. At most one in-memory transfer; discard on cancellation/replaced
begin. Apply through real EdgeConfigService; send explicit config.applied only AFTER
successful durable commit. On validation/storage failure send a bounded redacted
config.rejected matching incoming transfer identity; never claim applied. Inspect
existing receipt codecs and exact required fields. Do not marshal domain structs.
Unknown unsupported commands/telemetry/acks close with a fixed redacted error; don't
fake success. Full replay/current-state/commands remain M3. Preserve error sentinels
only when already redacted. Do not leak raw peer payload or close reasons.

Tests use actual websocket sessions (existing in-memory pipe helper possible),
real EdgeConfigService with mocks or SQLite adapter in external test package where
needed. Test valid handshake+health, wrong binding/stream/protocol/cursor/limits,
generation persist BEFORE config, stale/equal duplicate fails while incumbent stays
alive, higher generation cancels old and rejects late callbacks, cancellation/Close,
commit failure cannot emit applied, exact accepted bytes retained. Use existing
fixtures but adjust unsupported M2 features; do not weaken contract expectations.

Run focused race tests and probe lint. Prefix shell commands rtk, Go1.26.6. Avoid
whole-repo tests. Report exact commands/results, including unrelated current edits.
Read the prior reviews: no cancellation races, sender context must reach actual I/O,
no secret-bearing error wrapping, and verify effects rather than status codes.
