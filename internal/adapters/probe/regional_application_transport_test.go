package probe

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Transport-level tests for two-sided regional application reporting
// negotiation (docs/multi-region/M7_REGIONAL_APPLICATION_REPORTING.md).
//
// Every real-edge case runs the production pair over real pinned TLS/WebSocket:
// a real edge stack (runtime identity, single-use enrollment, encrypted edge
// SQLite, production EdgeConfigService.Apply/ReadRegionalApplications) against a
// real HubTransport.Run, so the welcome opt-in is computed by production code
// from the peer's hello and the configured hub repository, and the report path
// is the production frame handler. RemoteRegionalApplicationRepository is a
// narrow capturing double here; real-engine receipt acceptance is covered
// separately and remains the repository's authority.
//
// The unnegotiated cases are non-vacuous: each document is assignment-bound, so
// a wrongly enabled welcome would make the edge send a report that the hub must
// then reject, killing the session. Silence is therefore proven by leaving the
// session alive past the configuration ACK and observing fresh session traffic
// (a post-ACK committed observation replayed to the hub) plus a clean
// context.Canceled join, not by absence alone.

// regionalRepoCapture records repository calls. It is the narrowest possible
// RemoteRegionalApplicationRepository: the real engine semantics (committed
// receipt, immutable document binding, lease fencing) are its own tests.
type regionalRepoCapture struct {
	mu      sync.Mutex
	calls   int
	session domain.ProbeReplaySession
	facts   []domain.ProbeDeliveryModeApplication
	err     error
	called  chan struct{}
}

var _ ports.RemoteRegionalApplicationRepository = (*regionalRepoCapture)(nil)

func (r *regionalRepoCapture) RecordRemoteRegionalApplications(_ context.Context, session domain.ProbeReplaySession, applications []domain.ProbeDeliveryModeApplication) error {
	r.mu.Lock()
	r.calls++
	r.session = session
	r.facts = append([]domain.ProbeDeliveryModeApplication(nil), applications...)
	err := r.err
	r.mu.Unlock()
	select {
	case r.called <- struct{}{}:
	default:
	}
	return err
}

func (r *regionalRepoCapture) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *regionalRepoCapture) capture() (domain.ProbeReplaySession, []domain.ProbeDeliveryModeApplication) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.session, append([]domain.ProbeDeliveryModeApplication(nil), r.facts...)
}

// regionalBaseCapabilities is a legacy execution inventory: it advertises every
// capability the fixture document requires but never the reporting capability.
func regionalBaseCapabilities() []string {
	return []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"}
}

// regionalCredentialPrefix matches the production runtime credential shape.
const regionalCredentialPrefix = "phx_probe_"

// regionalRandomCredential mints a runtime credential fixture with real entropy
// so no test depends on a hard-coded secret-shaped literal.
func regionalRandomCredential(t *testing.T) string {
	t.Helper()
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return regionalCredentialPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
}

// regionalTransportHarness runs the real edge stack behind real pinned TLS and
// pairs it with a real HubTransport. Nothing on the report path is faked: the
// document is applied through the wire into encrypted edge SQLite, and the
// facts it reports are read back from that durable state.
type regionalTransportHarness struct {
	t           *testing.T
	id          *RuntimeIdentity
	store       *edge.Store
	transport   *HubTransport
	input       domain.ProbeSessionInput
	snapshot    ConfigSnapshot
	hash        string
	receipts    chan domain.ProbeActiveConfig
	ingested    chan domain.ProbeReplayBatch
	established atomic.Int64
}

func newRegionalTransportHarness(t *testing.T, desired domain.AlertDelivery, capabilities []string, repo ports.RemoteRegionalApplicationRepository) *regionalTransportHarness {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = id.Close() })
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint}, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	at := time.Now().UTC()
	enrollment := services.NewEdgeEnrollmentService(store)
	invite, err := enrollment.Issue(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	hubID := uuid.NewString()
	credential := regionalRandomCredential(t)
	enrollmentID := uuid.NewString()
	if err := enrollment.Accept(t.Context(), invite, credential, domain.EdgeEnrollment{HubID: hubID, ProbeID: id.ProbeID, EnrollmentID: enrollmentID, CredentialVersion: 1}, at); err != nil {
		t.Fatal(err)
	}
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(key[:])
	if err != nil {
		t.Fatal(err)
	}
	configs := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, err := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, err
	}, store, configs, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: capabilities}, func(ctx context.Context) (Health, error) {
		i, err := store.ReadIdentity(ctx)
		healthy, queue := true, int64(0)
		return Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &queue, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	// The replay outbox carries post-ACK session traffic used to prove the
	// unnegotiated cases kept a live, working session instead of dying silently.
	runtime.SetReplayRepository(store)
	handler, err := NewEdgeHTTPHandler(id, enrollment, runtime.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.Certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	connection := domain.ProbeCredentialMetadata{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID, EnrollmentID: enrollmentID, CredentialVersion: 1, Endpoint: "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1", Fingerprint: id.Fingerprint}
	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = hubID, id.ProbeID
	snapshot.Assignments[0].AlertDelivery = desired
	snapshot.Assignments[0].AssignmentRevision = Decimal(exactRegionalAssignmentRevision)
	document := configBytes(t, snapshot)
	digest := sha256.Sum256(document)
	transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if repo != nil {
		transport.SetRegionalApplications(repo)
	}
	return &regionalTransportHarness{
		t:         t,
		id:        id,
		store:     store,
		transport: transport,
		input:     domain.ProbeSessionInput{OwnerID: uuid.NewString(), Connection: connection, Token: credential, Generation: 1, ConfigDocument: document},
		snapshot:  snapshot,
		hash:      hex.EncodeToString(digest[:]),
		receipts:  make(chan domain.ProbeActiveConfig, 4),
		ingested:  make(chan domain.ProbeReplayBatch, 4),
	}
}

// wantFact is the exact durable regional fact the retained document must
// report: the effective regional mode and the immutable assignment-set binding,
// never the desired mode carried by the same document.
func (h *regionalTransportHarness) wantFact() domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: h.input.Connection.HubID, ProbeID: h.input.Connection.ProbeID},
		MonitorID:          h.snapshot.Assignments[0].MonitorID,
		AssignmentRevision: exactRegionalAssignmentRevision,
		Generation:         int64(h.snapshot.Assignments[0].Generation),
		ConfigRevision:     int64(h.snapshot.Revision),
		SHA256:             h.hash,
		StreamID:           h.input.Connection.StreamID,
		AlertDelivery:      domain.AlertDeliveryRegional,
	}
}

func (h *regionalTransportHarness) wantSession() domain.ProbeReplaySession {
	return domain.ProbeReplaySession{HubID: h.input.Connection.HubID, ProbeID: h.input.Connection.ProbeID, StreamID: h.input.Connection.StreamID, ConnectionGeneration: h.input.Generation, OwnerID: h.input.OwnerID}
}

// run starts one supervised hub session. The worker goroutine only reports the
// outcome; all failures surface on the test goroutine.
func (h *regionalTransportHarness) run(ctx context.Context) <-chan error {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	h.t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			h.t.Error("transport worker did not join during cleanup")
		}
	})
	go func() {
		defer close(done)
		done <- h.transport.Run(runCtx, h.input,
			func(context.Context) error {
				h.established.Add(1)
				return nil
			},
			func(_ context.Context, receipt domain.ProbeActiveConfig) error {
				h.receipts <- receipt
				return nil
			},
			func(_ context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
				h.ingested <- batch
				return &domain.ProbeReplayResult{StreamID: batch.StreamID, CommittedSeq: batch.LastSeq, AcceptedCount: int64(len(batch.Events))}, nil
			})
	}()
	return done
}

func (h *regionalTransportHarness) awaitReceipt(done <-chan error) domain.ProbeActiveConfig {
	h.t.Helper()
	select {
	case receipt := <-h.receipts:
		return receipt
	case err := <-done:
		h.t.Fatal("transport ended before configuration receipt", err)
		return domain.ProbeActiveConfig{}
	case <-time.After(10 * time.Second):
		h.t.Fatal("configuration receipt never arrived")
		return domain.ProbeActiveConfig{}
	}
}

// verifyPostACKTraffic commits one fresh observation after the configuration
// ACK and waits for it to be replayed to the hub, proving the session is alive
// and carrying traffic instead of merely having produced no report frame.
func (h *regionalTransportHarness) verifyPostACKTraffic() {
	h.t.Helper()
	now := time.Now().UTC()
	if _, err := h.store.CommitEdgeCheck(h.t.Context(), domain.EdgeCheckRecord{ExpectedStateSeq: 0, Observation: domain.RegionalObservation{MonitorID: h.snapshot.Assignments[0].MonitorID, AssignmentGeneration: int64(h.snapshot.Assignments[0].Generation), ProbeID: h.id.ProbeID, StreamID: h.id.StreamID, ConfigRevision: int64(h.snapshot.Revision), Status: domain.StatusUp, RawStatus: domain.StatusUp, DownCount: 0, Ping: 9, Message: "regional transport liveness", ObservedAt: now, ReceivedAt: now}}); err != nil {
		h.t.Fatal(err)
	}
	select {
	case batch := <-h.ingested:
		if batch.StreamID != h.input.Connection.StreamID {
			h.t.Fatal("post-ACK traffic lost the session stream identity")
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("session carried no traffic after the configuration ACK")
	}
}

func regionalAwaitRun(t *testing.T, done <-chan error, within time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		t.Fatal("transport session did not finish in time")
		return nil
	}
}

// TestRegionalTransportUnnegotiatedReportingStaysSilent proves the hub never
// opts in without the exact hello capability and a configured repository, and
// that "no report" is not a dead session: each case delivers its configuration
// ACK, then fresh replay traffic, then joins on cancellation. A wrong opt-in
// would make the assignment-bound edge send a report the hub must reject, which
// these liveness assertions cannot survive.
func TestRegionalTransportUnnegotiatedReportingStaysSilent(t *testing.T) {
	for _, test := range []struct {
		name    string
		desired domain.AlertDelivery
		extra   []string
		repo    bool
	}{
		{name: "legacy hello keeps configured repository unnegotiated", desired: domain.AlertDeliveryAggregate, repo: true},
		{name: "exact capability without repository stays unreported", desired: domain.AlertDeliveryBoth, extra: []string{RegionalApplicationCapability}},
		{name: "aggregate paging capability never substitutes", desired: domain.AlertDeliveryAggregate, extra: []string{"paging.aggregate.v1"}, repo: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			capture := &regionalRepoCapture{called: make(chan struct{}, 4)}
			var wired ports.RemoteRegionalApplicationRepository
			if test.repo {
				wired = capture
			}
			h := newRegionalTransportHarness(t, test.desired, append(regionalBaseCapabilities(), test.extra...), wired)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			done := h.run(ctx)
			receipt := h.awaitReceipt(done)
			if receipt.Revision != int64(h.snapshot.Revision) || receipt.SHA256 != h.hash || receipt.AssignmentCount != len(h.snapshot.Assignments) {
				t.Fatal("configuration receipt lost the document identity", receipt)
			}
			h.verifyPostACKTraffic()
			if h.established.Load() == 0 {
				t.Fatal("no health traffic crossed the live session")
			}
			if calls := capture.callCount(); calls != 0 {
				t.Fatalf("unnegotiated reporting reached the repository %d times", calls)
			}
			cancel()
			if err := regionalAwaitRun(t, done, 3*time.Second); !errors.Is(err, context.Canceled) {
				t.Fatalf("session died instead of staying live without reports: %v", err)
			}
			if calls := capture.callCount(); calls != 0 {
				t.Fatalf("unnegotiated reporting reached the repository %d times", calls)
			}
		})
	}
}

// TestRegionalTransportNegotiatedReportingEmitsPersistedRegionalFacts proves
// the exact capability plus a configured repository reports the facts the real
// encrypted edge store persisted for the exact retained document, even when
// that document desires aggregate or both paging. The reported fact is the
// effective regional mode with the full int64 assignment-set binding; it is
// read from durable SQLite state, never synthesized from desired assignments.
func TestRegionalTransportNegotiatedReportingEmitsPersistedRegionalFacts(t *testing.T) {
	for _, desired := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(desired), func(t *testing.T) {
			capture := &regionalRepoCapture{called: make(chan struct{}, 4)}
			h := newRegionalTransportHarness(t, desired, append(regionalBaseCapabilities(), RegionalApplicationCapability), capture)
			if h.snapshot.Assignments[0].AlertDelivery != desired {
				t.Fatal("fixture document does not carry the desired mode under test")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			done := h.run(ctx)
			receipt := h.awaitReceipt(done)
			if receipt.Revision != int64(h.snapshot.Revision) || receipt.SHA256 != h.hash {
				t.Fatal("configuration receipt lost the document identity", receipt)
			}
			select {
			case <-capture.called:
			case <-time.After(10 * time.Second):
				t.Fatal("negotiated regional report never reached the repository")
			}
			session, facts := capture.capture()
			want := []domain.ProbeDeliveryModeApplication{h.wantFact()}
			if calls := capture.callCount(); calls != 1 {
				t.Fatalf("repository recorded %d reports, want exactly 1", calls)
			}
			if session != h.wantSession() {
				t.Fatalf("report crossed with session authority %+v, want %+v", session, h.wantSession())
			}
			if !slices.Equal(facts, want) {
				t.Fatalf("reported facts %+v are not the persisted regional facts %+v", facts, want)
			}
			if facts[0].AssignmentRevision != exactRegionalAssignmentRevision {
				t.Fatal("the immutable assignment-set binding lost int64 precision on the wire")
			}
			if facts[0].AlertDelivery != domain.AlertDeliveryRegional {
				t.Fatal("the report leaked the desired mode instead of the effective regional mode")
			}
			// Reporting must not disturb the session: fresh traffic still flows
			// and the join stays caller-owned.
			h.verifyPostACKTraffic()
			cancel()
			if err := regionalAwaitRun(t, done, 3*time.Second); !errors.Is(err, context.Canceled) {
				t.Fatalf("session did not survive the accepted report: %v", err)
			}
		})
	}
}

// TestRegionalTransportRepositoryRefusalClosesWithRedactedError proves a
// repository failure ends the session with only the bounded public shape: the
// repository's private error text never crosses the transport boundary, and the
// refusal happens at the repository, not before it.
func TestRegionalTransportRepositoryRefusalClosesWithRedactedError(t *testing.T) {
	capture := &regionalRepoCapture{called: make(chan struct{}, 4), err: errors.New("private regional repository failure")}
	h := newRegionalTransportHarness(t, domain.AlertDeliveryAggregate, append(regionalBaseCapabilities(), RegionalApplicationCapability), capture)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	done := h.run(ctx)
	// The configuration ACK is written before the report, so the repository
	// refusal is the only thing that can end this session early.
	h.awaitReceipt(nil)
	err := regionalAwaitRun(t, done, 10*time.Second)
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		t.Fatalf("repository refusal did not close the session on its own: %v", err)
	}
	if strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "repository failure") {
		t.Fatal("repository failure leaked private detail across the transport:", err)
	}
	session, _ := capture.capture()
	if calls := capture.callCount(); calls != 1 {
		t.Fatalf("repository refused %d calls, want exactly 1", calls)
	}
	if session != h.wantSession() {
		t.Fatalf("refusal crossed with session authority %+v, want %+v", session, h.wantSession())
	}
}

// regionalControlledHandshake plays the probe role against a real hub session:
// it sends a hello advertising exactly the given capabilities and returns the
// raw welcome bytes so the test can assert whether the optional
// regional_application_reporting boolean was emitted at all.
func regionalControlledHandshake(ctx context.Context, t *testing.T, conn *websocket.Conn, s *controlledHubPeerSetup, capabilities []string) (Welcome, []byte) {
	t.Helper()
	hello := Hello{
		SessionIdentity:  SessionIdentity{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID, StreamID: s.input.Connection.StreamID},
		AgentVersion:     "test",
		ProtocolMin:      1,
		ProtocolMax:      1,
		ConfigRevision:   s.snapshot.Revision,
		FirstRetainedSeq: 1,
		LastCreatedSeq:   100,
		Capabilities:     capabilities,
		ResourceBindings: []ResourceBinding{},
	}
	helloFrame, err := encodeFrame("hello", 0, hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, helloFrame); err != nil {
		t.Fatal(err)
	}
	_, welcomeData, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("reading welcome: %v", err)
	}
	_, welcome, err := DecodeWelcome(welcomeData)
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	return welcome, welcomeData
}

// regionalControlledReportFrame builds a fully valid delivery.regional_applied
// frame bound to the controlled session identity and the exact document it
// carries. It would be accepted if negotiated, so any rejection is caused by
// the negotiation gate alone.
func regionalControlledReportFrame(t *testing.T, s *controlledHubPeerSetup) []byte {
	t.Helper()
	identity := SessionIdentity{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID, StreamID: s.input.Connection.StreamID}
	digest := sha256.Sum256(s.input.ConfigDocument)
	facts := []domain.ProbeDeliveryModeApplication{{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: identity.HubID, ProbeID: identity.ProbeID},
		MonitorID:          s.snapshot.Assignments[0].MonitorID,
		AssignmentRevision: exactRegionalAssignmentRevision,
		Generation:         int64(s.snapshot.Assignments[0].Generation),
		ConfigRevision:     int64(s.snapshot.Revision),
		SHA256:             hex.EncodeToString(digest[:]),
		StreamID:           identity.StreamID,
		AlertDelivery:      domain.AlertDeliveryRegional,
	}}
	reports, err := regionalApplicationReports(identity, facts)
	if err != nil || len(reports) != 1 {
		t.Fatal("controlled report could not be built:", err)
	}
	frame, err := encodeFrame("delivery.regional_applied", Decimal(s.input.Generation), reports[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeRegionalApplicationReport(frame); err != nil {
		t.Fatal("controlled report is not a valid frame:", err)
	}
	return frame
}

// TestRegionalTransportControlledProbeNegotiatesAndRejectsUnsolicitedFrames
// pins the hub's welcome computation at the wire with a controlled probe peer:
// the optional boolean is omitted unless the hello carries the exact compiled
// capability and a repository is configured, and a report sent without that
// opt-in is refused before the repository is ever called. The negotiated row
// proves the same frame is accepted and mapped once the opt-in exists.
func TestRegionalTransportControlledProbeNegotiatesAndRejectsUnsolicitedFrames(t *testing.T) {
	for _, row := range []struct {
		name          string
		capabilities  []string
		repo          bool
		wantOptIn     bool
		wantDelivered bool
	}{
		{name: "legacy hello keeps the repository unnegotiated", capabilities: regionalBaseCapabilities(), repo: true},
		{name: "aggregate paging capability never opts in", capabilities: append(regionalBaseCapabilities(), "paging.aggregate.v1"), repo: true},
		{name: "exact capability without repository stays unnegotiated", capabilities: append(regionalBaseCapabilities(), RegionalApplicationCapability)},
		{name: "exact capability with repository opts in and records the report", capabilities: append(regionalBaseCapabilities(), RegionalApplicationCapability), repo: true, wantOptIn: true, wantDelivered: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			s := setupControlledHubPeer(t)
			s.input.OwnerID = uuid.NewString()
			capture := &regionalRepoCapture{called: make(chan struct{}, 4)}
			if row.repo {
				s.transport.SetRegionalApplications(capture)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("controlled transport did not join")
				}
			}()
			go func() {
				defer close(done)
				done <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, nil)
			}()
			var conn *websocket.Conn
			select {
			case conn = <-s.connCh:
			case err := <-done:
				t.Fatal("controlled handshake never started:", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			defer func() { _ = conn.CloseNow() }()
			welcome, welcomeRaw := regionalControlledHandshake(ctx, t, conn, s, row.capabilities)
			if welcome.RegionalApplicationReporting != row.wantOptIn {
				t.Fatalf("welcome opted in = %v, want %v", welcome.RegionalApplicationReporting, row.wantOptIn)
			}
			if emitted := bytes.Contains(welcomeRaw, []byte(`"regional_application_reporting"`)); emitted != row.wantOptIn {
				t.Fatalf("welcome emitted the optional boolean = %v, want %v (absence means false)", emitted, row.wantOptIn)
			}
			frame := regionalControlledReportFrame(t, s)
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				t.Fatal(err)
			}
			if row.wantDelivered {
				select {
				case <-capture.called:
				case <-ctx.Done():
					t.Fatal("negotiated report never reached the repository")
				}
				session, facts := capture.capture()
				digest := sha256.Sum256(s.input.ConfigDocument)
				want := []domain.ProbeDeliveryModeApplication{{
					ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID},
					MonitorID:          s.snapshot.Assignments[0].MonitorID,
					AssignmentRevision: exactRegionalAssignmentRevision,
					Generation:         int64(s.snapshot.Assignments[0].Generation),
					ConfigRevision:     int64(s.snapshot.Revision),
					SHA256:             hex.EncodeToString(digest[:]),
					StreamID:           s.input.Connection.StreamID,
					AlertDelivery:      domain.AlertDeliveryRegional,
				}}
				if calls := capture.callCount(); calls != 1 {
					t.Fatalf("repository recorded %d reports, want exactly 1", calls)
				}
				if session != (domain.ProbeReplaySession{HubID: s.input.Connection.HubID, ProbeID: s.input.Connection.ProbeID, StreamID: s.input.Connection.StreamID, ConnectionGeneration: s.input.Generation, OwnerID: s.input.OwnerID}) {
					t.Fatal("report crossed with the wrong session authority", session)
				}
				if !slices.Equal(facts, want) {
					t.Fatalf("mapped facts %+v do not match the wire entries %+v", facts, want)
				}
				// An accepted report must leave the session running and caller-owned.
				cancel()
				if err := regionalAwaitRun(t, done, 3*time.Second); !errors.Is(err, context.Canceled) {
					t.Fatalf("accepted report disturbed the session: %v", err)
				}
				return
			}
			// Unnegotiated: the session must close on the frame itself, well
			// before the bounded deadline, with the repository untouched.
			if err := regionalAwaitRun(t, done, 5*time.Second); err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
				t.Fatalf("unsolicited report did not end the session by itself: %v", err)
			}
			if calls := capture.callCount(); calls != 0 {
				t.Fatalf("unsolicited report reached the repository %d times", calls)
			}
		})
	}
}
