package probe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
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
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type lostCertificateResult struct {
	*edge.Store
	lose      atomic.Bool
	failRead  atomic.Bool
	committed chan domain.ProbeCommandOutcome
	reads     atomic.Int32
	readBlock <-chan struct{}
}

func (r *lostCertificateResult) ApplyCertificateCommand(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand) (domain.ProbeCommandOutcome, error) {
	out, err := r.Store.ApplyCertificateCommand(ctx, a, c)
	if err == nil && c.Kind == CommandCertificateActivate && r.lose.Swap(false) {
		r.committed <- out
		return domain.ProbeCommandOutcome{}, errors.New("injected certificate result loss after commit")
	}
	return out, err
}

func (r *lostCertificateResult) ReadActiveCertificate(ctx context.Context) (domain.EdgeCertificateState, error) {
	r.reads.Add(1)
	if r.readBlock != nil {
		select {
		case <-r.readBlock:
		case <-ctx.Done():
			return domain.EdgeCertificateState{}, ctx.Err()
		}
	}
	if r.failRead.Load() {
		return domain.EdgeCertificateState{}, errors.New("injected certificate read failure")
	}
	return r.Store.ReadActiveCertificate(ctx)
}

func TestCertificateRuntimeConcurrentRecoverySharesReconciliation(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	before := f.certificateCommands.reads.Load()
	block := make(chan struct{})
	f.certificateCommands.readBlock = block
	f.manager.active.Store(nil)
	var workers sync.WaitGroup
	started := make(chan struct{}, 32)
	errors := make(chan error, 32)
	for range 32 {
		workers.Go(func() { started <- struct{}{}; _, _, err := f.manager.CurrentIdentity(t.Context()); errors <- err })
	}
	for range 32 {
		<-started
	}
	// Hold the first real SQLite read while a burst reaches the recovery gate.
	time.Sleep(50 * time.Millisecond)
	close(block)
	workers.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := f.certificateCommands.reads.Load() - before; got != 1 {
		t.Fatalf("one missing cache caused %d durable reconciliations", got)
	}
}

func TestCertificateRuntimeActivationKeepsAdmissionDeadline(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	c, payload := f.prepareCertificate(3 * time.Second)
	f.send(first, 1, payload)
	prepared := f.result(first)
	details, ok := prepared.Details.(CertificatePrepareDetails)
	if !ok {
		t.Fatal("missing prepare details")
	}
	second := f.dial(f.oldToken, 2)
	f.send(second, 2, f.activationPayload(c, details.TLSFingerprint))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, frame, err := second.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := DecodeCommandResult(frame)
	if err != nil || result.Status != "applied" {
		t.Fatal("missing activation receipt", err)
	}
	f.closed(second, 4*time.Second)
	f.setPin(details.TLSFingerprint)
	_ = f.dial(f.oldToken, 3)
}

type certificateRuntimeFixture struct {
	*credentialRuntimeFixture
	protector           *auth.ProbeConfigProtector
	manager             *EdgeTLSManager
	certificateCommands *lostCertificateResult
	httpServer          *http.Server
	serveDone           chan error
}

func newCertificateRuntimeFixture(t *testing.T) *certificateRuntimeFixture {
	t.Helper()
	f := &certificateRuntimeFixture{credentialRuntimeFixture: &credentialRuntimeFixture{t: t, dir: t.TempDir(), hubID: uuid.NewString()}}
	if err := os.Chmod(f.dir, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.id, err = InitializeRuntimeIdentity(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.stop(); _ = f.id.Close() })
	f.protector, err = auth.NewProbeConfigProtector(bytes.Repeat([]byte{91}, 32))
	if err != nil {
		t.Fatal(err)
	}
	f.oldToken = "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{92}, 32))
	f.openStore()
	servicesNewEdgeEnrollmentForCommandTest(t, f.store, f.hubID, f.oldToken)
	f.start()
	return f
}

func (f *certificateRuntimeFixture) openStore() {
	f.t.Helper()
	material, err := NewEdgeCertificateMaterial(f.protector)
	if err != nil {
		f.t.Fatal(err)
	}
	f.store, err = edge.Open(f.t.Context(), f.dir, domain.EdgeIdentity{ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, Fingerprint: f.id.Fingerprint}, edge.WithCertificateMaterial(material))
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *certificateRuntimeFixture) start() {
	f.t.Helper()
	var err error
	f.certificateCommands = &lostCertificateResult{Store: f.store, committed: make(chan domain.ProbeCommandOutcome, 1)}
	f.manager, err = NewEdgeTLSManager(f.t.Context(), f.id, f.certificateCommands, f.protector)
	if err != nil {
		f.t.Fatal(err)
	}
	// Advertise certificate support independently: unrelated command capabilities
	// must not be required for certificate dispatch.
	configs := services.NewEdgeConfigService(f.store, f.store, NewEdgeConfigDecoder(checker.Get, notifier.Get), f.protector)
	f.runtime, err = NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, e := f.store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, e
	}, f.store, configs, EdgeRuntimeConfig{AgentVersion: "certificate-test", Capabilities: []string{"snapshot.v1", CertificateRotationCapability}}, func(context.Context) (Health, error) {
		healthy, q := true, int64(0)
		return Health{Role: "probe", Ready: false, DBWritable: true, SchedulerHealthy: &healthy, QueueBytes: &q, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.runtime.SetCredentialCommands(f.store)
	f.runtime.SetCertificateCommands(f.manager)
	handler, err := NewManagedEdgeHTTPHandler(f.id, services.NewEdgeEnrollmentService(f.store), f.runtime.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} }, f.manager)
	if err != nil {
		f.t.Fatal(err)
	}
	address := "127.0.0.1:0"
	if f.endpoint != "" {
		address = strings.TrimSuffix(strings.TrimPrefix(f.endpoint, "wss://"), "/ws/probe/v1")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		f.t.Fatal(err)
	}
	f.httpServer = &http.Server{Handler: handler, TLSConfig: f.manager.TLSConfig(), ConnContext: f.manager.ConnContext, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	f.serveDone = make(chan error, 1)
	server, done := f.httpServer, f.serveDone
	go func() { done <- server.ServeTLS(listener, "", "") }()
	f.endpoint = "wss://" + listener.Addr().String() + "/ws/probe/v1"
	pin, _, err := f.manager.CurrentIdentity(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	f.setPin(pin)
}

func (f *certificateRuntimeFixture) setPin(pin string) {
	f.t.Helper()
	if f.client != nil {
		f.client.CloseIdleConnections()
	}
	var err error
	f.client, err = NewPinnedHTTPClient(f.endpoint, pin, EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *certificateRuntimeFixture) stop() {
	if f.runtime != nil {
		_ = f.runtime.Close()
		f.runtime = nil
	}
	if f.httpServer != nil {
		_ = f.httpServer.Close()
		f.httpServer = nil
	}
	if f.serveDone != nil {
		<-f.serveDone
		f.serveDone = nil
	}
	if f.client != nil {
		f.client.CloseIdleConnections()
		f.client = nil
	}
	if f.store != nil {
		_ = f.store.Close()
		f.store = nil
	}
}

func (f *certificateRuntimeFixture) prepareCertificate(window time.Duration) (domain.ProbeCertificateCommand, []byte) {
	f.t.Helper()
	created := time.Now().UTC().Truncate(time.Microsecond).Add(-domain.ProbeCredentialOverlap + window)
	c := domain.ProbeCertificateCommand{CommandID: uuid.NewString(), ProbeID: f.id.ProbeID, Kind: CommandCertificatePrepare, CreatedAt: created, ExpiresAt: created.Add(time.Hour), RotationID: uuid.NewString(), CertificateVersion: 2, ValidForDays: 365}
	payload, err := (CertificateCommandCodec{}).EncodeCertificateCommand(f.t.Context(), c)
	if err != nil {
		f.t.Fatal(err)
	}
	return c, payload
}

func (f *certificateRuntimeFixture) activationPayload(c domain.ProbeCertificateCommand, pin string) []byte {
	f.t.Helper()
	c.CommandID, c.Kind, c.ValidForDays, c.ExpectedFingerprint = uuid.NewString(), CommandCertificateActivate, 0, pin
	payload, err := (CertificateCommandCodec{}).EncodeCertificateCommand(f.t.Context(), c)
	if err != nil {
		f.t.Fatal(err)
	}
	return payload
}

func (f *certificateRuntimeFixture) rejectPin(pin string) {
	f.t.Helper()
	client, err := NewPinnedHTTPClient(f.endpoint, pin, EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		f.t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, f.endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + f.oldToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if conn != nil {
		_ = conn.CloseNow()
	}
	if err == nil {
		f.t.Fatal("wrong certificate pin connected")
	}
}

func TestCertificateRuntimeLostActivationReplyAndColdRestart(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	manifest, err := os.ReadFile(filepath.Join(f.dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	tlsFile, err := os.ReadFile(filepath.Join(f.dir, "tls.pem"))
	if err != nil {
		t.Fatal(err)
	}
	first := f.dial(f.oldToken, 1)
	c, payload := f.prepareCertificate(4 * time.Second)
	f.send(first, 1, payload)
	prepared := f.result(first)
	details, ok := prepared.Details.(CertificatePrepareDetails)
	if !ok || prepared.Status != "applied" || details.CertificateVersion != 2 {
		t.Fatal("invalid prepare receipt", prepared)
	}
	f.rejectPin(details.TLSFingerprint)
	second := f.dial(f.oldToken, 2)
	activate := f.activationPayload(c, details.TLSFingerprint)
	f.certificateCommands.lose.Store(true)
	f.send(second, 2, activate)
	var source domain.ProbeCommandOutcome
	select {
	case source = <-f.certificateCommands.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not commit")
	}
	f.closed(second, 5*time.Second)
	f.rejectPin(f.id.Fingerprint)
	f.stop()
	if err := f.id.Close(); err != nil {
		t.Fatal(err)
	}
	f.id, err = OpenRuntimeIdentityAnchor(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.openStore()
	f.start()
	timer := time.NewTimer(time.Until(c.CreatedAt.Add(domain.ProbeCredentialOverlap).Add(100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	third := f.dial(f.oldToken, 3)
	f.send(third, 3, activate)
	result := f.result(third)
	if result.Status != "applied" || result.AppliedAt == nil || !time.Time(*result.AppliedAt).Equal(*source.AppliedAt) {
		t.Fatal("lost activation changed original receipt after restart and retirement", result)
	}
	fourth := f.dial(f.oldToken, 4)
	f.send(fourth, 4, payload)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, receiptFrame, err := fourth.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := DecodeCommandResult(receiptFrame)
	if err != nil || !reflect.DeepEqual(got, prepared) {
		t.Fatal("cold restart changed preparation receipt")
	}
	// This is the active new certificate, after the old overlap has retired.
	// Recovering its original prepare receipt must still allow the hub to commit.
	graceCtx, endGrace := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer endGrace()
	_, _, readErr := fourth.Read(graceCtx)
	if readErr == nil || graceCtx.Err() == nil {
		t.Fatal("historical prepare receipt prematurely closed a current identity", readErr)
	}
	pin, _, err := f.manager.CurrentIdentity(t.Context())
	if err != nil || pin != details.TLSFingerprint || pin == f.id.Fingerprint {
		t.Fatal("restart did not select durable certificate", err)
	}
	afterManifest, err := os.ReadFile(filepath.Join(f.dir, "identity.json"))
	if err != nil {
		t.Fatal(err)
	}
	afterTLS, err := os.ReadFile(filepath.Join(f.dir, "tls.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, afterManifest) || !bytes.Equal(tlsFile, afterTLS) {
		t.Fatal("rotation overwrote immutable bootstrap files")
	}
}

func TestCertificateRuntimePreparationBoundsExistingSession(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	connection := f.dial(f.oldToken, 1)
	c, payload := f.prepareCertificate(2 * time.Second)
	f.send(connection, 1, payload)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, frame, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, prepared, err := DecodeCommandResult(frame)
	if err != nil || prepared.Status != "applied" {
		t.Fatal("missing preparation receipt", err)
	}
	details, ok := prepared.Details.(CertificatePrepareDetails)
	if !ok {
		t.Fatal("missing preparation details")
	}
	// Leave the first result unconfirmed. An existing socket must stop effects
	// immediately and close by the fixed overlap, even when less than 12s remain.
	f.send(connection, 1, f.activationPayload(c, details.TLSFingerprint))
	f.closed(connection, 4*time.Second)
	state, err := f.store.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 1 {
		t.Fatal("quiescent socket activated candidate", err)
	}
	_ = f.dial(f.oldToken, 2)
}

func TestCertificateRuntimeRejectsHandshakeDelayedPastRetirement(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	c, payload := f.prepareCertificate(4 * time.Second)
	f.send(first, 1, payload)
	prepared := f.result(first)
	details, ok := prepared.Details.(CertificatePrepareDetails)
	if !ok {
		t.Fatal("missing preparation details")
	}
	transport, ok := f.client.Transport.(*pinnedRoundTripper)
	if !ok {
		t.Fatal("unexpected pinned transport")
	}
	address := strings.TrimSuffix(strings.TrimPrefix(f.endpoint, "wss://"), "/ws/probe/v1")
	raw, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, transport.transport.TLSClientConfig.Clone())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	second := f.dial(f.oldToken, 2)
	f.send(second, 2, f.activationPayload(c, details.TLSFingerprint))
	f.result(second)
	timer := time.NewTimer(time.Until(c.CreatedAt.Add(domain.ProbeCredentialOverlap).Add(100 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	oldTransport := &http.Transport{DialTLSContext: func(context.Context, string, string) (net.Conn, error) { return raw, nil }}
	defer oldTransport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, f.endpoint, &websocket.DialOptions{HTTPClient: &http.Client{Transport: oldTransport}, HTTPHeader: http.Header{"Authorization": {"Bearer " + f.oldToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal("old TLS handshake did not reach runtime admission", err)
	}
	defer func() { _ = conn.CloseNow() }()
	_, helloFrame, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, hello, err := DecodeHello(helloFrame)
	if err != nil {
		t.Fatal(err)
	}
	welcome := Welcome{SessionIdentity: hello.SessionIdentity, SelectedProtocol: 1, ConnectionGeneration: 3, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())}
	frame, err := encodeFrame("welcome", 3, welcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil || ctx.Err() != nil {
		t.Fatal("retired TLS handshake obtained healthy admission", err)
	}
	i, err := f.store.ReadIdentity(t.Context())
	if err != nil || i.ConnectionGeneration != 2 {
		t.Fatal("retired handshake advanced durable generation", i, err)
	}
	f.setPin(details.TLSFingerprint)
	current := f.dial(f.oldToken, 3)
	_ = current.CloseNow()
}

func TestCertificateRuntimeFailedReconciliationFailsClosedAndRecovers(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	c, payload := f.prepareCertificate(time.Minute)
	f.send(first, 1, payload)
	prepared := f.result(first)
	details, ok := prepared.Details.(CertificatePrepareDetails)
	if !ok {
		t.Fatal("missing prepare details")
	}
	second := f.dial(f.oldToken, 2)
	f.certificateCommands.failRead.Store(true)
	activate := f.activationPayload(c, details.TLSFingerprint)
	f.send(second, 2, activate)
	f.closed(second, 5*time.Second)
	state, err := f.store.ReadActiveCertificate(t.Context())
	if err != nil || state.ActiveVersion != 2 {
		t.Fatal("fault missed committed activation", state, err)
	}
	// Neither a stale old cache nor unverified candidate may serve while durable
	// reconciliation is unavailable. Recovery reads the original committed row.
	f.rejectPin(f.id.Fingerprint)
	f.rejectPin(details.TLSFingerprint)
	f.certificateCommands.failRead.Store(false)
	f.setPin(details.TLSFingerprint)
	third := f.dial(f.oldToken, 3)
	f.send(third, 3, activate)
	if result := f.result(third); result.Status != "applied" {
		t.Fatal("reconciliation lost original result", result)
	}
}

func TestCertificateRuntimeResumptionDoesNotBypassSelection(t *testing.T) {
	f := newCertificateRuntimeFixture(t)
	transport, ok := f.client.Transport.(*pinnedRoundTripper)
	if !ok {
		t.Fatal("unexpected pinned adapter")
	}
	config := transport.transport.TLSClientConfig.Clone()
	config.ClientSessionCache = tls.NewLRUClientSessionCache(2)
	address := strings.TrimSuffix(strings.TrimPrefix(f.endpoint, "wss://"), "/ws/probe/v1")
	for range 2 {
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 3 * time.Second}, "tcp", address, config)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(conn, "GET /healthz HTTP/1.1\r\nHost: "+address+"\r\nConnection: close\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, readErr := io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		resumed := conn.ConnectionState().DidResume
		_ = conn.Close()
		if readErr != nil || response.StatusCode != http.StatusOK || resumed {
			t.Fatal("handshake bypassed certificate selection", resumed, readErr)
		}
	}
}
