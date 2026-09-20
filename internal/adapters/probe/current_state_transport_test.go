package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
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

type stateApplyFunc func(context.Context, domain.ProbeReplaySession, domain.ProbeCurrentSnapshot) (*domain.ProbeStateReceipt, error)

func (f stateApplyFunc) ApplySnapshot(ctx context.Context, s domain.ProbeReplaySession, p domain.ProbeCurrentSnapshot) (*domain.ProbeStateReceipt, error) {
	return f(ctx, s, p)
}

func TestHubHealthConfirmsConfigImmediatelyAfterDurableReceipt(t *testing.T) {
	s := setupControlledHubPeer(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	recording, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(ctx context.Context, _ domain.ProbeActiveConfig) error {
			close(recording)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return nil, domain.ErrInternal
		})
	}()
	defer func() { cancel(); <-done }()
	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = conn.CloseNow() }()
	performHandshakeAndConfig(ctx, t, conn, s)
	select {
	case <-recording:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	healths := make(chan Health, 16)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				return
			}
			_, h, err := DecodeHealth(data)
			if err == nil {
				select {
				case healths <- h:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	defer func() { cancel(); _ = conn.CloseNow(); <-readerDone }()
	quiet := time.NewTimer(50 * time.Millisecond)
beforeCommit:
	for {
		select {
		case h := <-healths:
			if h.ConfigRevision != 0 {
				t.Fatal("uncommitted config advertised as applied", h)
			}
		case <-quiet.C:
			break beforeCommit
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(release)
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case h := <-healths:
			if h.ConfigRevision == s.snapshot.Revision && h.Ready && h.DBWritable {
				return
			}
		case <-deadline.C:
			t.Fatal("durable config receipt waited for periodic health instead of waking current state")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestHubTransportCurrentStateReceiptRequiresCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "failed"}[fail], func(t *testing.T) {
			s := setupControlledHubPeer(t)
			s.input.OwnerID = uuid.NewString()
			seen := make(chan domain.ProbeCurrentSnapshot, 1)
			s.transport.SetStateIngest(stateApplyFunc(func(ctx context.Context, authority domain.ProbeReplaySession, candidate domain.ProbeCurrentSnapshot) (*domain.ProbeStateReceipt, error) {
				if authority.OwnerID != s.input.OwnerID || authority.ConnectionGeneration != s.input.Generation {
					return nil, errors.New("incorrect session authority")
				}
				seen <- candidate
				if fail {
					return nil, domain.ErrInternal
				}
				return &domain.ProbeStateReceipt{SnapshotID: candidate.SnapshotID, StreamID: candidate.StreamID, ConfigRevision: candidate.ConfigRevision, SHA256: candidate.SHA256, StateCount: len(candidate.States), AppliedAt: time.Now().UTC()}, ctx.Err()
			}))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, nil)
			}()
			var conn *websocket.Conn
			select {
			case conn = <-s.connCh:
			case err := <-done:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			defer func() { _ = conn.CloseNow() }()
			conn.SetReadLimit(MaxFrameBytes)
			performHandshakeAndConfig(ctx, t, conn, s)
			snapshot := StateSnapshot{StreamID: s.input.Connection.StreamID, ConfigRevision: s.snapshot.Revision, CreatedAt: Timestamp(time.Now().UTC()), LastCreatedSeq: 20, States: []MonitorState{}}
			// Empty snapshots are meaningful: the repository reconciles all omissions.
			document, err := json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err = DecodeStateSnapshot(document)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := prepareStateReceipt(snapshot, document)
			if err != nil {
				t.Fatal(err)
			}
			send := func(kind string, payload any) {
				t.Helper()
				frame, err := encodeFrame(kind, 1, payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
			}
			send("state.begin", StateBegin{StateTransferIdentity: receipt.StateTransferIdentity, StateSchemaVersion: 1, CreatedAt: snapshot.CreatedAt, LastCreatedSeq: 20, TotalBytes: len(document), ChunkCount: 1, SHA256: receipt.SHA256})
			send("state.chunk", StateChunk{StateTransferIdentity: receipt.StateTransferIdentity, Index: 0, Data: document})
			send("state.commit", StateCommit{StateTransferIdentity: receipt.StateTransferIdentity, SHA256: receipt.SHA256})
			env, data, err := readReplayResponse(ctx, conn)
			if fail {
				if err == nil {
					t.Fatalf("failed commit emitted %s", env.Type)
				}
			} else {
				if err != nil || env.Type != "state.applied" {
					t.Fatalf("state receipt missing: %s %v", env.Type, err)
				}
				_, applied, err := DecodeStateApplied(data)
				if err != nil || !sameStateReceipt(receipt, applied) {
					t.Fatalf("incorrect state receipt: %+v %v", applied, err)
				}
			}
			select {
			case candidate := <-seen:
				if candidate.LastCreatedSeq != 20 || candidate.ProbeID != s.input.Connection.ProbeID {
					t.Fatal("state mapping lost authority")
				}
			case <-ctx.Done():
				t.Fatal("state callback missing")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("state transport did not join")
			}
		})
	}
}

func TestEdgeRuntimeCurrentStatePrecedesAndRefreshesDuringBacklog(t *testing.T) {
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
	configs := services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	if err := store.AcceptConnectionGeneration(t.Context(), hubID, 1); err != nil {
		t.Fatal(err)
	}
	config := m2Config(t)
	config.HubID, config.ProbeID = hubID, id.ProbeID
	if _, err := configs.Apply(t.Context(), configBytes(t, config), 1, at); err != nil {
		t.Fatal(err)
	}
	assignment := config.Assignments[0]
	var previous int64
	record := func(status domain.Status) {
		t.Helper()
		down := 0
		if status == domain.StatusDown {
			down = 1
		}
		o, err := store.CommitEdgeCheck(t.Context(), domain.EdgeCheckRecord{ExpectedStateSeq: previous, Observation: domain.RegionalObservation{MonitorID: assignment.MonitorID, AssignmentGeneration: int64(assignment.Generation), ProbeID: id.ProbeID, StreamID: id.StreamID, ConfigRevision: int64(config.Revision), Status: status, RawStatus: status, DownCount: down, Ping: 9, Message: "current snapshot source", ObservedAt: time.Now().UTC(), ReceivedAt: time.Now().UTC()}})
		if err != nil {
			t.Fatal(err)
		}
		previous = o.Seq
	}
	for n := 0; n < 300; n++ {
		record(domain.StatusUp)
	}
	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		d, e := store.ReadDiagnostics(ctx)
		return d.Identity, d.FirstRetainedSeq, e
	}, store, configs, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"}}, func(ctx context.Context) (Health, error) {
		d, e := store.ReadDiagnostics(ctx)
		healthy := true
		var oldest *Timestamp
		if d.OldestQueuedAt != nil {
			v := Timestamp(*d.OldestQueuedAt)
			oldest = &v
		}
		return Health{Role: "probe", Ready: true, DBWritable: true, SchedulerHealthy: &healthy, ConfigRevision: Decimal(d.Identity.ConfigRevision), CommittedSeq: Decimal(d.Identity.CommittedSeq), QueueBytes: &d.QueueBytes, OldestQueuedAt: oldest, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}}, e
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = runtime.Close() }()
	runtime.SetReplayRepository(store)
	runtime.SetStateRepository(store)
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
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + runtimeToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	conn.SetReadLimit(MaxFrameBytes)
	_, hello, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeHello(hello); err != nil {
		t.Fatal(err)
	}
	send := func(kind string, payload any) {
		t.Helper()
		frame, err := encodeFrame(kind, 2, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
			t.Fatal(err)
		}
	}
	send("welcome", Welcome{SessionIdentity: SessionIdentity{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID}, SelectedProtocol: 1, ConnectionGeneration: 2, DesiredConfigRevision: config.Revision, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())})
	send("health", Health{Role: "hub", Ready: true, DBWritable: true, ConfigRevision: config.Revision, ClockTime: Timestamp(time.Now().UTC()), Errors: []string{}})
	var transfer *StateTransfer
	snapshots := 0
	historySeen := false
	for snapshots < 2 {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		env, err := DecodeEnvelope(data)
		if err != nil {
			t.Fatal(err)
		}
		switch env.Type {
		case "health":
		case "state.begin":
			if transfer != nil {
				t.Fatal("overlapping state transfer")
			}
			transfer, err = NewStateTransfer(data, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		case "state.chunk":
			if transfer == nil {
				t.Fatal("state chunk without begin")
			}
			if err := transfer.AddChunk(data, time.Now()); err != nil {
				t.Fatal(err)
			}
		case "state.commit":
			if transfer == nil {
				t.Fatal("state commit without begin")
			}
			candidate, err := transfer.Commit(data, time.Now())
			transfer = nil
			if err != nil {
				t.Fatal(err)
			}
			snapshots++
			if len(candidate.States) != 1 || candidate.LastCreatedSeq != Decimal(299+snapshots) || candidate.States[0].LastObservationSeq != candidate.LastCreatedSeq {
				t.Fatalf("state not coherent: %+v", candidate)
			}
			if snapshots == 1 && historySeen {
				t.Fatal("history preceded initial durable state")
			}
			if snapshots == 2 && (!historySeen || candidate.States[0].Status != "DOWN") {
				t.Fatal("fresh state did not overtake stalled backlog")
			}
			identity, err := store.ReadIdentity(t.Context())
			if err != nil || identity.CommittedSeq != 0 {
				t.Fatal("state receipt pruned unacknowledged history", identity, err)
			}
			_, commit, err := DecodeStateCommit(data)
			if err != nil {
				t.Fatal(err)
			}
			send("state.applied", StateApplied{StateTransferIdentity: commit.StateTransferIdentity, SHA256: commit.SHA256, AppliedAt: Timestamp(time.Now().UTC()), StateCount: 1})
		case "telemetry.batch":
			if snapshots == 0 {
				t.Fatal("initial state gate failed")
			}
			historySeen = true
			if previous == 300 {
				record(domain.StatusDown)
			}
			// Hold history ACKs: periodic current state must still make progress.
		default:
			t.Fatalf("unexpected source frame %s", env.Type)
		}
	}
	cancel()
	_ = conn.CloseNow()
}
