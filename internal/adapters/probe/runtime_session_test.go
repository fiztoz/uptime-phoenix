package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
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
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
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
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint}, edge.WithTelemetryEncoder(EdgeTelemetryEncoder{}))
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
	}, store, configs, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1", AcknowledgementCapability, "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"}}, func(ctx context.Context) (Health, error) {
		i, err := store.ReadIdentity(ctx)
		healthy, queue := true, int64(0)
		return Health{Role: "probe", Ready: i.ConfigRevision > 0, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(i.ConfigRevision), QueueBytes: &queue, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, err
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime.SetCommands(store)
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
	// Also attempt the same well-formed commit on the retained old socket.
	// Transport refusal is expected because takeover already closed it. This
	// is distinct from stale-envelope validation on the live new socket below.
	oldCommit, err := encodeFrame("config.commit", 1, ConfigCommit{ConfigTransferIdentity: transferID, SHA256: hash})
	if err != nil {
		t.Fatal(err)
	}
	oldCtx, oldCancel := context.WithTimeout(t.Context(), time.Second)
	oldWriteErr := first.Write(oldCtx, websocket.MessageText, oldCommit)
	oldCancel()
	if oldWriteErr == nil {
		if _, oldReadErr := read(first); oldReadErr == nil || errors.Is(oldReadErr, context.DeadlineExceeded) {
			t.Fatal("retained old socket did not reject commit promptly")
		}
	} else if errors.Is(oldWriteErr, context.DeadlineExceeded) {
		t.Fatal("old socket rejection was only a local timeout")
	}
	if oldIdentity, oldErr := store.ReadIdentity(t.Context()); oldErr != nil || oldIdentity.ConfigRevision != 0 || oldIdentity.ConnectionGeneration != 2 {
		t.Fatal("old socket attempt changed durable state")
	}
	// Stage a fully valid document on the replacement TLS session, then send
	// its commit with the superseded generation. The payload is identical to
	// the successful control below: only the envelope authority is stale.
	staleBegin := ConfigBegin{ConfigTransferIdentity: transferID, ConfigSchemaVersion: 1, TotalBytes: len(document), ChunkCount: 1, SHA256: hash, RequiredCapabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"}, EffectiveAt: snapshot.EffectiveAt}
	write(second, "config.begin", 2, staleBegin)
	write(second, "config.chunk", 2, ConfigChunk{ConfigTransferIdentity: transferID, Index: 0, Data: document})
	write(second, "config.commit", 1, ConfigCommit{ConfigTransferIdentity: transferID, SHA256: hash})
	for {
		response, readErr := read(second)
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) {
				t.Fatal("stale commit rejection was only a local timeout")
			}
			break
		}
		if response.Type != "health" {
			t.Fatalf("stale-generation commit produced %s rather than closing the session", response.Type)
		}
	}
	i, err = store.ReadIdentity(t.Context())
	if err != nil || i.ConfigRevision != 0 || i.ConnectionGeneration != 2 {
		t.Fatalf("stale commit changed durable configuration or authority: %+v %v", i, err)
	}
	if _, activeErr := store.ReadActiveConfig(t.Context()); !errors.Is(activeErr, ports.ErrNotFound) {
		t.Fatal("stale commit persisted an active document")
	}
	// Establish fresh authority and replay the same exact document/commit as
	// a positive control. This is in-process TLS/SQLite, not a native AWS run.
	second = dial(3, nil)
	if h, readErr := read(second); readErr != nil || h.Type != "health" {
		t.Fatal("fresh authority did not establish after stale commit")
	}
	transfer := func() string {
		t.Helper()
		begin := ConfigBegin{ConfigTransferIdentity: transferID, ConfigSchemaVersion: 1, TotalBytes: len(document), ChunkCount: 1, SHA256: hash, RequiredCapabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1", "proxy.socks5.v1"}, EffectiveAt: snapshot.EffectiveAt}
		write(second, "config.begin", 3, begin)
		write(second, "config.chunk", 3, ConfigChunk{ConfigTransferIdentity: transferID, Index: 0, Data: document})
		write(second, "config.commit", 3, ConfigCommit{ConfigTransferIdentity: transferID, SHA256: hash})
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
	// Seed one isolated firing incident (no scheduler or provider executes in
	// this fixture). Validate that an otherwise valid ACK cannot run with a
	// stale envelope, and that the identical payload succeeds with authority.
	resolved, err := configs.Load(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	a := resolved.Assignments[0]
	now := time.Now().UTC().Truncate(time.Microsecond)
	sourceID := uuid.NewString()
	incident := domain.RegionalIncident{SourceAlertID: sourceID, ProbeID: id.ProbeID, MonitorID: a.Monitor.ID, AssignmentGeneration: a.Generation, ConfigRevision: resolved.Metadata.Revision, Scope: domain.IncidentScopeRegional, SubjectKind: domain.IncidentSubjectAvailability, Status: domain.AlertStatusFiring, TransitionVersion: 1, StartedAt: now}
	_, err = store.CommitEdgeCheck(t.Context(), domain.EdgeCheckRecord{Observation: domain.RegionalObservation{ProbeID: id.ProbeID, StreamID: id.StreamID, MonitorID: a.Monitor.ID, AssignmentGeneration: a.Generation, ConfigRevision: resolved.Metadata.Revision, Status: domain.StatusDown, RawStatus: domain.StatusDown, DownCount: 1, Important: true, ObservedAt: now, ReceivedAt: now}, Incident: &incident})
	if err != nil {
		t.Fatal(err)
	}
	ack := domain.ProbeAlertAcknowledgement{CommandID: uuid.NewString(), ProbeID: id.ProbeID, SourceAlertID: sourceID, AssignmentGeneration: a.Generation, ActorDisplayName: "Test operator", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}
	payload, err := (AcknowledgementCodec{}).EncodeAcknowledgement(t.Context(), ack)
	if err != nil {
		t.Fatal(err)
	}
	inspect, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edge.db")+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inspect.Close() }()
	receiptCount := func() int {
		t.Helper()
		var count int
		if err := inspect.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM edge_applied_commands WHERE command_id = ?", ack.CommandID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if receiptCount() != 0 {
		t.Fatal("command receipt existed before attempt")
	}
	before, err := store.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sendCommand := func(generation Decimal) {
		t.Helper()
		frame, err := encodeCommandRequestFrame(generation, payload)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		if err := second.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatal(err)
		}
	}
	sendCommand(2) // live socket belongs to generation 3
	for {
		response, readErr := read(second)
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) {
				t.Fatal("stale command rejection was only timeout")
			}
			break
		}
		if response.Type != "health" {
			t.Fatalf("stale command produced %s", response.Type)
		}
	}
	after, err := store.ReadIdentity(t.Context())
	if err != nil || after.LastCreatedSeq != before.LastCreatedSeq || after.ConnectionGeneration != 3 || receiptCount() != 0 {
		t.Fatal("stale command mutated durable state", err)
	}
	evidence, err := store.ReadEdgeEvidence(t.Context(), a.Monitor.ID, a.Generation)
	if err != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusFiring || evidence.Incident.TransitionVersion != 1 {
		t.Fatal("stale command changed incident", err)
	}
	second = dial(4, nil)
	if h, readErr := read(second); readErr != nil || h.Type != "health" {
		t.Fatal("command control session unavailable")
	}
	sendCommand(4)
	for {
		response, readErr := read(second)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.Type == "health" {
			continue
		}
		var outcome CommandResult
		if response.Type != "command.result" || json.Unmarshal(response.Payload, &outcome) != nil || outcome.CommandID != ack.CommandID || outcome.Status != "applied" {
			t.Fatal("valid command positive control failed")
		}
		break
	}
	evidence, err = store.ReadEdgeEvidence(t.Context(), a.Monitor.ID, a.Generation)
	after, identityErr := store.ReadIdentity(t.Context())
	if err != nil || identityErr != nil || evidence.Incident == nil || evidence.Incident.Status != domain.AlertStatusAcked || evidence.Incident.TransitionVersion != 2 || receiptCount() != 1 || after.LastCreatedSeq != before.LastCreatedSeq+1 {
		t.Fatal("positive control lacked one durable receipt/transition", err, identityErr)
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
