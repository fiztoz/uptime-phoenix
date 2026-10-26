package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
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

// Transport-level coverage for the protected cutover commands: a real edge
// stack (runtime identity, single-use enrollment, encrypted edge SQLite, a real
// configured cutover executor hook) paired with a real HubTransport over pinned
// TLS/WebSocket. The welcome negotiation is computed by production code, the
// command dispatch and receipt transport are the production frame handlers, and
// the executor observes the one captured decoded command exactly once.

// cutoverTransportDispatcher hands out one queued payload per claim and records
// every transported receipt. It is a narrow ProbeCommandDispatcher double: real
// dispatch journaling is the hub repository's own authority.
type cutoverTransportDispatcher struct {
	mu       sync.Mutex
	payloads [][]byte
	results  chan domain.ProbeCommandOutcome
}

var _ ports.ProbeCommandDispatcher = (*cutoverTransportDispatcher)(nil)

func (d *cutoverTransportDispatcher) NextCommand(context.Context, domain.ProbeReplaySession, time.Duration, domain.ProbeCommandCapabilities) (*domain.ProbeCommandDispatch, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.payloads) == 0 {
		return nil, nil
	}
	payload := d.payloads[0]
	d.payloads = d.payloads[1:]
	return &domain.ProbeCommandDispatch{Payload: bytes.Clone(payload)}, nil
}

func (d *cutoverTransportDispatcher) RecordCommandResult(_ context.Context, _ domain.ProbeReplaySession, outcome domain.ProbeCommandOutcome) (bool, error) {
	d.results <- outcome
	return false, nil
}

type cutoverPair struct {
	transport *HubTransport
	runtime   *EdgeRuntime
	server    *httptest.Server
	metadata  domain.ProbeCredentialMetadata
	token     string
	document  []byte
	spy       *cutoverExecutorSpy
	dispatch  *cutoverTransportDispatcher
}

func newCutoverPair(t *testing.T, negotiated bool) *cutoverPair {
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
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{51}, 32))
	if err != nil {
		t.Fatal(err)
	}
	hubID := uuid.NewString()
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{52}, 32))
	enrollment := servicesNewEdgeEnrollmentForCommandTest(t, store, hubID, token)
	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = hubID, id.ProbeID
	document := configBytes(t, snapshot)
	spy := &cutoverExecutorSpy{}
	configs := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, e := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, e
	}, store, configs, EdgeRuntimeConfig{
		AgentVersion: "cutover-test",
		Capabilities: []string{"snapshot.v1", DeliveryCutoverCapability, "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"},
		// The strong capability is advertised exactly because this real hook is
		// configured; the negotiated gate is exercised through the welcome.
		DeliveryCutoverExecutor: spy,
	}, func(ctx context.Context) (Health, error) {
		i, e := store.ReadIdentity(ctx)
		// Honest readiness: DecodeHealth rejects a ready probe without an active
		// configuration, and the first config arrives on the session itself.
		healthy, q := true, int64(0)
		return Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &q, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, e
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Close() })
	sourceErr := make(chan error, 8)
	handler, err := NewEdgeHTTPHandler(id, services.NewEdgeEnrollmentService(store), func(ctx context.Context, conn *websocket.Conn, binding domain.EdgeEnrollment) error {
		err := runtime.Handle(ctx, conn, binding)
		t.Logf("source session ended: %v", err)
		select {
		case sourceErr <- err:
		default:
		}
		return err
	}, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.Certificate}}
	server.StartTLS()
	t.Cleanup(server.Close)
	metadata := domain.ProbeCredentialMetadata{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID, EnrollmentID: enrollment.EnrollmentID, CredentialVersion: 1, Fingerprint: id.Fingerprint}
	metadata.Endpoint = "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1"
	dispatch := &cutoverTransportDispatcher{results: make(chan domain.ProbeCommandOutcome, 8)}
	transport := NewHubTransport(EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	transport.SetCommands(dispatch)
	transport.SetDeliveryCutover(negotiated)
	return &cutoverPair{transport: transport, runtime: runtime, server: server, metadata: metadata, token: token, document: document, spy: spy, dispatch: dispatch}
}

// run opens one session at the given generation and returns its result stream.
func (p *cutoverPair) run(t *testing.T, ctx context.Context, generation int64, payloads [][]byte) <-chan error {
	t.Helper()
	p.dispatch.mu.Lock()
	p.dispatch.payloads = append([][]byte(nil), payloads...)
	p.dispatch.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		done <- p.transport.Run(ctx, domain.ProbeSessionInput{Connection: p.metadata, Token: p.token, Generation: generation, OwnerID: uuid.NewString(), ConfigDocument: p.document}, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return nil, errors.New("unexpected replay in control-only fixture")
		})
	}()
	return done
}

func (p *cutoverPair) awaitOutcome(t *testing.T, ctx context.Context, done <-chan error) domain.ProbeCommandOutcome {
	t.Helper()
	select {
	case outcome := <-p.dispatch.results:
		return outcome
	case err := <-done:
		t.Fatalf("session ended before a command receipt: %v", err)
		return domain.ProbeCommandOutcome{}
	case <-ctx.Done():
		t.Fatal("command receipt never arrived")
		return domain.ProbeCommandOutcome{}
	}
}

// TestDeliveryCutoverTransportDeliversCommandsAndReceipts proves the negotiated
// path end to end: each queued kind is delivered once, the real executor hook
// observes exactly the decoded command under the accepted session authority, and
// its kind-specific receipt is transported back through production framing.
func TestDeliveryCutoverTransportDeliversCommandsAndReceipts(t *testing.T) {
	p := newCutoverPair(t, true)
	quiesce, quiescePayload := deliveryCutoverFixture(t, CommandDeliveryQuiesce)
	apply, applyPayload := deliveryCutoverFixture(t, CommandDeliveryApply)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := p.run(t, ctx, 2, [][]byte{quiescePayload, applyPayload})
	first := p.awaitOutcome(t, ctx, done)
	second := p.awaitOutcome(t, ctx, done)
	want := map[string]domain.ProbeCommandOutcome{
		domain.DeliveryQuiescedCode: {CommandID: quiesce.CommandID, Status: "applied"},
		domain.DeliveryAppliedCode:  {CommandID: apply.CommandID, Status: "applied"},
	}
	for _, outcome := range []domain.ProbeCommandOutcome{first, second} {
		expect, ok := want[outcome.Code]
		if !ok || outcome.CommandID != expect.CommandID || outcome.Status != expect.Status || outcome.AppliedAt == nil {
			t.Fatalf("transported receipt changed: %+v", outcome)
		}
	}
	if p.spy.count() != 2 {
		t.Fatalf("executor ran %d times for 2 delivered commands", p.spy.count())
	}
	calls := p.spy.last()
	if calls.Kind != CommandDeliveryApply || calls.AssignmentRevision != 9007199254740995 || calls.MonitorID != 9007199254740993 {
		t.Fatalf("exact command precision lost at the executor: %+v", calls.ProbeDeliveryModeApplication)
	}
	authority := p.spy.lastAuthority()
	if authority.HubID != p.metadata.HubID || authority.ProbeID != p.metadata.ProbeID || authority.StreamID != p.metadata.StreamID || authority.ConnectionGeneration != 2 {
		t.Fatalf("executor authority is not the accepted session: %+v", authority)
	}
	cancel()
	_ = <-done
}

// TestDeliveryCutoverTransportDrainKeepsResumeDeliverable proves the
// feature-disable drain: with the welcome unnegotiated, a new quiesce is refused
// without a receipt and without killing the session, while delivery.resume still
// reaches the configured executor and transports its receipt in that same live
// session — a source hold can never be stranded by feature disable.
func TestDeliveryCutoverTransportDrainKeepsResumeDeliverable(t *testing.T) {
	p := newCutoverPair(t, false)
	_, quiescePayload := deliveryCutoverFixture(t, CommandDeliveryQuiesce)
	resume, resumePayload := deliveryCutoverFixture(t, CommandDeliveryResume)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	done := p.run(t, ctx, 2, [][]byte{quiescePayload, resumePayload})
	outcome := p.awaitOutcome(t, ctx, done)
	if outcome.CommandID != resume.CommandID || outcome.Code != domain.DeliveryResumedCode || outcome.Status != "applied" || outcome.AppliedAt == nil {
		t.Fatalf("drain-time resume receipt changed: %+v", outcome)
	}
	if p.spy.count() != 1 || p.spy.last().Kind != CommandDeliveryResume {
		t.Fatalf("unnegotiated quiesce was executed or resume skipped: %d calls", p.spy.count())
	}
	// The session survived the refused quiesce: the resumed receipt arrived on
	// the same generation and no fabricated completion was transported.
	select {
	case extra := <-p.dispatch.results:
		t.Fatalf("fabricated receipt transported: %+v", extra)
	case <-time.After(250 * time.Millisecond):
	}
	cancel()
	_ = <-done
}
