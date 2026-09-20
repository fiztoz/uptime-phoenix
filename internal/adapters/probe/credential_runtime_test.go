package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"strings"
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

type lostCredentialResult struct {
	*edge.Store
	lose      atomic.Bool
	committed chan domain.ProbeCommandOutcome
}

func (r *lostCredentialResult) ApplyCredentialCommand(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeCredentialCommand) (domain.ProbeCommandOutcome, error) {
	out, err := r.Store.ApplyCredentialCommand(ctx, a, c)
	if err == nil && c.Kind == CommandCredentialActivate && r.lose.Swap(false) {
		r.committed <- out
		return domain.ProbeCommandOutcome{}, errors.New("injected result loss after source commit")
	}
	return out, err
}

type credentialRuntimeFixture struct {
	t        *testing.T
	dir      string
	id       *RuntimeIdentity
	store    *edge.Store
	runtime  *EdgeRuntime
	server   *httptest.Server
	client   *http.Client
	endpoint string
	hubID    string
	oldToken string
	newToken string
	commands *lostCredentialResult
}

func newCredentialRuntimeFixture(t *testing.T) *credentialRuntimeFixture {
	t.Helper()
	f := &credentialRuntimeFixture{t: t, dir: t.TempDir(), hubID: uuid.NewString()}
	if err := os.Chmod(f.dir, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.id, err = InitializeRuntimeIdentity(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.stop(); _ = f.id.Close() })
	f.oldToken = "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{73}, 32))
	f.newToken = "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{74}, 32))
	f.openStore()
	servicesNewEdgeEnrollmentForCommandTest(t, f.store, f.hubID, f.oldToken)
	f.start()
	return f
}

func (f *credentialRuntimeFixture) openStore() {
	f.t.Helper()
	var err error
	f.store, err = edge.Open(f.t.Context(), f.dir, domain.EdgeIdentity{ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, Fingerprint: f.id.Fingerprint})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *credentialRuntimeFixture) start() {
	f.t.Helper()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{75}, 32))
	if err != nil {
		f.t.Fatal(err)
	}
	configs := services.NewEdgeConfigService(f.store, f.store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	f.runtime, err = NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, e := f.store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, e
	}, f.store, configs, EdgeRuntimeConfig{AgentVersion: "credential-test", Capabilities: []string{"snapshot.v1", CredentialRotationCapability}}, func(context.Context) (Health, error) {
		healthy, queue := true, int64(0)
		return Health{Role: "probe", Ready: false, DBWritable: true, SchedulerHealthy: &healthy, QueueBytes: &queue, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.commands = &lostCredentialResult{Store: f.store, committed: make(chan domain.ProbeCommandOutcome, 1)}
	f.runtime.SetCredentialCommands(f.commands)
	handler, err := NewEdgeHTTPHandler(f.id, services.NewEdgeEnrollmentService(f.store), f.runtime.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		f.t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(handler)
	if f.endpoint != "" {
		// A durable candidate binds the endpoint as well as its certificate.
		// Reopen the same listener for restart tests instead of changing trust.
		_ = f.server.Listener.Close()
		address := strings.TrimSuffix(strings.TrimPrefix(f.endpoint, "wss://"), "/ws/probe/v1")
		f.server.Listener, err = net.Listen("tcp", address)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{f.id.Certificate}}
	f.server.StartTLS()
	f.endpoint = "wss" + strings.TrimPrefix(f.server.URL, "https") + "/ws/probe/v1"
	f.client, err = NewPinnedHTTPClient(f.endpoint, f.id.Fingerprint, EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *credentialRuntimeFixture) stop() {
	if f.runtime != nil {
		_ = f.runtime.Close()
	}
	if f.server != nil {
		f.server.Close()
	}
	if f.client != nil {
		f.client.CloseIdleConnections()
	}
	if f.store != nil {
		_ = f.store.Close()
	}
}

func (f *credentialRuntimeFixture) dial(token string, generation Decimal) *websocket.Conn {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, f.endpoint, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		f.t.Fatal("pinned credential dial failed", err)
	}
	f.t.Cleanup(func() { _ = conn.CloseNow() })
	_, data, err := conn.Read(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	_, hello, err := DecodeHello(data)
	if err != nil || hello.StreamID != f.id.StreamID {
		f.t.Fatal("invalid credential hello", err)
	}
	welcome := Welcome{SessionIdentity: SessionIdentity{HubID: f.hubID, ProbeID: f.id.ProbeID, StreamID: f.id.StreamID}, SelectedProtocol: 1, ConnectionGeneration: generation, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())}
	frame, err := encodeFrame("welcome", generation, welcome)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		f.t.Fatal(err)
	}
	_, data, err = conn.Read(ctx)
	if err != nil {
		f.t.Fatal("credential admission failed", err)
	}
	if _, _, err := DecodeHealth(data); err != nil {
		f.t.Fatal("admission lacked health", err)
	}
	return conn
}

func (f *credentialRuntimeFixture) prepare(window time.Duration) ([]byte, CommandRequest) {
	f.t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	c := CommandRequest{CommandID: uuid.NewString(), Kind: CommandCredentialPrepare, CreatedAt: Timestamp(now), ExpiresAt: Timestamp(now.Add(time.Minute)), Target: CommandTarget{ProbeID: f.id.ProbeID}, Data: CredentialPrepareData{RotationID: uuid.NewString(), CredentialVersion: 2, Token: f.newToken, OverlapExpiresAt: Timestamp(now.Add(window))}}
	payload, err := json.Marshal(c)
	if err != nil {
		f.t.Fatal(err)
	}
	// Exact request identity survives encoding/reconnect, including whitespace.
	payload = bytes.Replace(payload, []byte(`,"kind"`), []byte(",\n \"kind\""), 1)
	return payload, c
}

func (f *credentialRuntimeFixture) send(conn *websocket.Conn, generation Decimal, payload []byte) {
	f.t.Helper()
	frame, err := encodeCommandRequestFrame(generation, payload)
	if err != nil {
		f.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		f.t.Fatal(err)
	}
}

func (f *credentialRuntimeFixture) result(conn *websocket.Conn) CommandResult {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 5*time.Second)
	defer cancel()
	_, frame, err := conn.Read(ctx)
	if err != nil {
		f.t.Fatal("missing rotation receipt", err)
	}
	_, result, err := DecodeCommandResult(frame)
	if err != nil {
		f.t.Fatal(err)
	}
	// The test peer has accepted the result; a real hub closes after its receipt commits.
	_ = conn.CloseNow()
	return result
}

func (f *credentialRuntimeFixture) closed(conn *websocket.Conn, within time.Duration) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), within)
	defer cancel()
	_, _, err := conn.Read(ctx)
	if err == nil || ctx.Err() != nil {
		f.t.Fatal("source failed to close credential session before deadline", err)
	}
}

func TestCredentialRuntimeLostActivationReplyRecoversWithNewIdentity(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	payload, prepare := f.prepare(time.Minute)
	f.send(first, 1, payload)
	prepared := f.result(first)
	if prepared.Status != "applied" || prepared.Details != (CredentialPrepareDetails{CredentialVersion: 2}) {
		t.Fatal("wrong preparation receipt", prepared.Status)
	}
	// Prepared identity works before activation; it is not activation proof.
	second := f.dial(f.newToken, 2)
	data := prepare.Data.(CredentialPrepareData)
	activate := prepare
	activate.CommandID, activate.Kind, activate.Data = uuid.NewString(), CommandCredentialActivate, CredentialActivateData{RotationID: data.RotationID, CredentialVersion: 2}
	activationPayload, err := json.Marshal(activate)
	if err != nil {
		t.Fatal(err)
	}
	f.commands.lose.Store(true)
	f.send(second, 2, activationPayload)
	var original domain.ProbeCommandOutcome
	select {
	case original = <-f.commands.committed:
	case <-time.After(5 * time.Second):
		t.Fatal("activation not committed")
	}
	if original.Status != "applied" {
		t.Fatal("lost reply was not an applied result")
	}
	f.closed(second, time.Second)
	f.stop()
	f.openStore()
	f.start()
	third := f.dial(f.newToken, 3)
	f.send(third, 3, activationPayload)
	recovered := f.result(third)
	out, err := commandOutcome(recovered)
	if err != nil || !reflect.DeepEqual(out, original) {
		t.Fatal("lost activation receipt changed after restart", err)
	}
	// A successful receipt recovery still forces a fresh authenticated session.
	_ = f.dial(f.newToken, 4)
	binding, err := f.store.ReadEnrollment(t.Context())
	if err != nil || binding.CredentialVersion != 2 {
		t.Fatal("new identity not durably promoted", err)
	}
	i, err := f.store.ReadIdentity(t.Context())
	if err != nil || i.LastCreatedSeq != 0 || i.CommittedSeq != 0 || i.StreamID != f.id.StreamID {
		t.Fatal("rotation changed telemetry identity", err)
	}
}

func TestCredentialRuntimePreparedSessionExpiresAndOriginalRecovers(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	payload, _ := f.prepare(2 * time.Second)
	f.send(first, 1, payload)
	if result := f.result(first); result.Status != "applied" {
		t.Fatal(result.Status)
	}
	pending := f.dial(f.newToken, 2)
	f.closed(pending, 4*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, f.endpoint, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Authorization": {"Bearer " + f.newToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatal("expired prepared token still authenticated")
	}
	_ = f.dial(f.oldToken, 3)
}

func TestCredentialRuntimePreviousSessionExpiresAfterActivation(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	first := f.dial(f.oldToken, 1)
	payload, c := f.prepare(3 * time.Second)
	f.send(first, 1, payload)
	_ = f.result(first)
	second := f.dial(f.newToken, 2)
	data := c.Data.(CredentialPrepareData)
	c.CommandID, c.Kind, c.Data = uuid.NewString(), CommandCredentialActivate, CredentialActivateData{RotationID: data.RotationID, CredentialVersion: 2}
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	f.send(second, 2, payload)
	if result := f.result(second); result.Status != "applied" {
		t.Fatal(result.Status)
	}
	previous := f.dial(f.oldToken, 3)
	f.closed(previous, 5*time.Second)
	_, valid, err := services.NewEdgeEnrollmentService(f.store).AuthenticateRuntime(t.Context(), f.oldToken)
	if err != nil || valid {
		t.Fatal("old credential survived fixed overlap", err)
	}
	_ = f.dial(f.newToken, 4)
}

func TestCredentialRuntimeUnconfirmedResultQuiescesAndCloses(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	connection := f.dial(f.oldToken, 1)
	payload, c := f.prepare(time.Minute)
	f.send(connection, 1, payload)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, frame, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, result, err := DecodeCommandResult(frame)
	if err != nil || result.Status != "applied" {
		t.Fatal("prepare receipt missing", err)
	}
	// Unlike a cooperating hub, leave the socket open and send another effect.
	// It must not execute while the source awaits receipt confirmation/reconnect.
	data := c.Data.(CredentialPrepareData)
	c.CommandID, c.Kind, c.Data = uuid.NewString(), CommandCredentialActivate, CredentialActivateData{RotationID: data.RotationID, CredentialVersion: 2}
	payload, err = json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	f.send(connection, 1, payload)
	start := time.Now()
	f.closed(connection, 15*time.Second)
	if time.Since(start) < 10*time.Second {
		t.Fatal("source did not allow the hub's bounded receipt commit")
	}
	binding, err := f.store.ReadEnrollment(t.Context())
	if err != nil || binding.CredentialVersion != 1 {
		t.Fatal("quiescent connection executed activation", err)
	}
	fresh := f.dial(f.newToken, 2)
	f.send(fresh, 2, payload)
	if result := f.result(fresh); result.Status != "applied" {
		t.Fatal("fresh authenticated generation could not activate", result.Status)
	}
}

func TestCredentialRuntimePreparationBoundsExistingSession(t *testing.T) {
	f := newCredentialRuntimeFixture(t)
	connection := f.dial(f.oldToken, 1)
	payload, _ := f.prepare(2 * time.Second)
	f.send(connection, 1, payload)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, frame, err := connection.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, receipt, err := DecodeCommandResult(frame)
	if err != nil || receipt.Status != "applied" {
		t.Fatal("missing preparation receipt", err)
	}
	f.closed(connection, 4*time.Second)
	_ = f.dial(f.oldToken, 2)
}
