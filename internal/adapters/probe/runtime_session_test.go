package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
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

type runtimeConfigStore struct {
	*edge.Store
	reject atomic.Bool
}

func (s *runtimeConfigStore) ActivateConfig(ctx context.Context, c domain.EdgeActiveConfig) error {
	if s.reject.Load() {
		return errors.New("private database failure")
	}
	return s.Store.ActivateConfig(ctx, c)
}

func TestEdgeRuntimeFencingConfigReceiptAndShutdown(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = id.Close() }()
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	enrollment := services.NewEdgeEnrollmentService(store)
	at := time.Now().UTC()
	token, err := enrollment.Issue(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	hubID := uuid.NewString()
	runtimeToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	if err := enrollment.Accept(t.Context(), token, runtimeToken, domain.EdgeEnrollment{HubID: hubID, ProbeID: id.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1}, at); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	configStore := &runtimeConfigStore{Store: store}
	configs := services.NewEdgeConfigService(store, configStore, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, err := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, err
	}, store, configs, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"}}, func(ctx context.Context) (Health, error) {
		i, err := store.ReadIdentity(ctx)
		healthy, queue := true, int64(0)
		return Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &queue, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	handler, err := NewEdgeHTTPHandler(id, enrollment, runtime.Handle, func(context.Context) EdgeReadiness { return EdgeReadiness{} })
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{id.Certificate}}
	server.StartTLS()
	defer server.Close()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1"
	client, err := NewPinnedHTTPClient(endpoint, id.Fingerprint, EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}})
	if err != nil {
		t.Fatal(err)
	}
	write := func(conn *websocket.Conn, kind string, generation Decimal, payload any) {
		t.Helper()
		frame, err := encodeFrame(kind, generation, payload)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatal(err)
		}
	}
	read := func(conn *websocket.Conn) (Envelope, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		kind, data, err := conn.Read(ctx)
		if err != nil {
			return Envelope{}, err
		}
		if kind != websocket.MessageText {
			t.Fatal("binary runtime frame")
		}
		return DecodeEnvelope(data)
	}
	dial := func(generation Decimal, mutate func(*Welcome)) *websocket.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + runtimeToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
		env, err := read(conn)
		if err != nil || env.Type != "hello" || env.ConnectionGeneration != 0 {
			t.Fatal("missing hello")
		}
		welcome := Welcome{SessionIdentity: SessionIdentity{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID}, SelectedProtocol: 1, ConnectionGeneration: generation, DesiredConfigRevision: 12, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())}
		if mutate != nil {
			mutate(&welcome)
		}
		write(conn, "welcome", generation, welcome)
		return conn
	}
	first := dial(1, nil)
	if h, err := read(first); err != nil || h.Type != "health" {
		t.Fatalf("no established health: %+v %v", h, err)
	}
	for name, mutate := range map[string]func(*Welcome){"duplicate": nil, "wrong stream": func(w *Welcome) { w.StreamID = uuid.NewString() }, "cursor ahead": func(w *Welcome) { w.CommittedSeq = 1 }} {
		t.Run(name, func(t *testing.T) {
			conn := dial(1, mutate)
			if _, err := read(conn); err == nil {
				t.Fatal("invalid newcomer established")
			}
		})
	}
	transferID := ConfigTransferIdentity{SnapshotID: uuid.NewString(), Revision: 12}
	write(first, "config.commit", 1, ConfigCommit{ConfigTransferIdentity: transferID, SHA256: strings.Repeat("a", 64)})
	if response, err := read(first); err != nil || response.Type != "config.rejected" {
		t.Fatal("invalid newcomer displaced valid incumbent")
	}
	second := dial(2, nil)
	if h, err := read(second); err != nil || h.Type != "health" {
		t.Fatal("replacement not established")
	}
	if _, err := read(first); err == nil {
		t.Fatal("old session remained live")
	}
	i, err := store.ReadIdentity(t.Context())
	if err != nil || i.ConnectionGeneration != 2 {
		t.Fatal("health preceded durable fence")
	}
	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = hubID, id.ProbeID
	document := configBytes(t, snapshot)
	digest := sha256.Sum256(document)
	hash := hex.EncodeToString(digest[:])
	transfer := func() string {
		t.Helper()
		begin := ConfigBegin{ConfigTransferIdentity: transferID, ConfigSchemaVersion: 1, TotalBytes: len(document), ChunkCount: 1, SHA256: hash, RequiredCapabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"}, EffectiveAt: snapshot.EffectiveAt}
		write(second, "config.begin", 2, begin)
		write(second, "config.chunk", 2, ConfigChunk{ConfigTransferIdentity: transferID, Index: 0, Data: document})
		write(second, "config.commit", 2, ConfigCommit{ConfigTransferIdentity: transferID, SHA256: hash})
		for {
			response, err := read(second)
			if err != nil {
				t.Fatal(err)
			}
			if response.Type != "health" {
				return response.Type
			}
		}
	}
	configStore.reject.Store(true)
	if response := transfer(); response != "config.rejected" {
		t.Fatalf("failed commit replied %s", response)
	}
	i, err = store.ReadIdentity(t.Context())
	if err != nil || i.ConfigRevision != 0 {
		t.Fatal("failed activation changed revision")
	}
	configStore.reject.Store(false)
	if response := transfer(); response != "config.applied" {
		t.Fatalf("valid commit replied %s", response)
	}
	active, err := store.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	plain, err := protector.Open(t.Context(), active.Snapshot.ProbeConfigMetadata, active.Snapshot.ProtectedPayload)
	if err != nil || !bytes.Equal(plain, document) {
		t.Fatal("applied receipt preceded exact-byte persistence")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := read(second); err == nil {
		t.Fatal("runtime shutdown left session alive")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal("repeated close failed")
	}
}
