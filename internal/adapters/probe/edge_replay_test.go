package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
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
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type fakeReplayRepo struct {
	mu             sync.Mutex
	readBatchFunc  func(ctx context.Context, fromSeq int64, maxEvents int, maxBytes int) (*domain.EdgeReplayBatch, error)
	commitACKFunc  func(ctx context.Context, fence domain.EdgeReplayFence, result domain.ProbeReplayResult) error
	readBatchCalls int
	commitACKCalls int
	lastFence      domain.EdgeReplayFence
	lastResult     domain.ProbeReplayResult
}

func (f *fakeReplayRepo) ReadReplayBatch(ctx context.Context, fromSeq int64, maxEvents int, maxBytes int) (*domain.EdgeReplayBatch, error) {
	f.mu.Lock()
	f.readBatchCalls++
	fn := f.readBatchFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, fromSeq, maxEvents, maxBytes)
	}
	return &domain.EdgeReplayBatch{
		StreamID: uuid.NewString(),
		FirstSeq: 1,
		LastSeq:  0,
		Items:    nil,
	}, nil
}

func (f *fakeReplayRepo) CommitReplayACK(ctx context.Context, fence domain.EdgeReplayFence, result domain.ProbeReplayResult) error {
	f.mu.Lock()
	f.commitACKCalls++
	f.lastFence = fence
	f.lastResult = result
	fn := f.commitACKFunc
	f.mu.Unlock()
	if fn != nil {
		return fn(ctx, fence, result)
	}
	return nil
}

func sampleObservationJSON(seq int64, monitorID int64, extraWhitespace string) []byte {
	return []byte(fmt.Sprintf(`{
  "seq": "%d",
  "kind": "observation",
  "observed_at": "2026-09-20T12:00:00Z",
  "data": {
    "monitor_id": %d,
    "assignment_generation": "1",
    "config_revision": "1",
    "status": "UP",
    "raw_status": "UP",
    "down_count": 0,
    "ping": 10,
    "duration_ms": 15,
    "message": "all good",
    "important": false,
    "conditions": [],
    "tls": null
  }%s
}`, seq, monitorID, extraWhitespace))
}

func TestEdgeReplayPump_ExactBytesPreservation(t *testing.T) {
	streamID := uuid.NewString()
	observedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	// Payload with deliberate whitespace, tabs, and trailing newlines.
	rawPayload1 := sampleObservationJSON(1, 101, "\n  \t\n")
	rawPayload2 := sampleObservationJSON(2, 102, "  ")

	batch := &domain.EdgeReplayBatch{
		StreamID: streamID,
		FirstSeq: 1,
		LastSeq:  2,
		Items: []domain.EdgeReplayItem{
			{Seq: 1, Kind: "observation", ObservedAt: observedAt, Payload: rawPayload1},
			{Seq: 2, Kind: "observation", ObservedAt: observedAt, Payload: rawPayload2},
		},
		TotalBytes: len(rawPayload1) + len(rawPayload2),
	}

	frame, err := buildReplayBatchFrame(streamID, 1, batch)
	if err != nil {
		t.Fatalf("buildReplayBatchFrame failed: %v", err)
	}

	// Verify the exact raw bytes are preserved as substrings in the frame.
	if !bytes.Contains(frame, rawPayload1) {
		t.Fatalf("frame does not contain exact rawPayload1 bytes")
	}
	if !bytes.Contains(frame, rawPayload2) {
		t.Fatalf("frame does not contain exact rawPayload2 bytes")
	}

	// Verify framing decodes cleanly through envelope and telemetry batch decoders.
	env, tb, err := DecodeTelemetryBatch(frame)
	if err != nil {
		t.Fatalf("DecodeTelemetryBatch failed: %v", err)
	}
	if env.Type != "telemetry.batch" || env.ConnectionGeneration != 1 {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	if tb.StreamID != streamID || tb.FirstSeq != 1 || tb.LastSeq != 2 || len(tb.Events) != 2 {
		t.Fatalf("unexpected telemetry batch: %+v", tb)
	}
}

func TestEdgeReplayPump_BuildReplayBatchFrameValidation(t *testing.T) {
	streamID := uuid.NewString()
	observedAt := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	validPayload := sampleObservationJSON(1, 101, "")

	tests := []struct {
		name      string
		streamID  string
		gen       Decimal
		batch     *domain.EdgeReplayBatch
		wantError string
	}{
		{
			name:      "nil batch",
			streamID:  streamID,
			gen:       1,
			batch:     nil,
			wantError: "validation",
		},
		{
			name:      "empty streamID",
			streamID:  "",
			gen:       1,
			batch:     &domain.EdgeReplayBatch{StreamID: streamID, FirstSeq: 1, LastSeq: 1, Items: []domain.EdgeReplayItem{{Seq: 1, Payload: validPayload}}},
			wantError: "stream ID mismatch",
		},
		{
			name:      "mismatched streamID",
			streamID:  streamID,
			gen:       1,
			batch:     &domain.EdgeReplayBatch{StreamID: uuid.NewString(), FirstSeq: 1, LastSeq: 1, Items: []domain.EdgeReplayItem{{Seq: 1, Payload: validPayload}}},
			wantError: "stream ID mismatch",
		},
		{
			name:      "non-positive generation",
			streamID:  streamID,
			gen:       0,
			batch:     &domain.EdgeReplayBatch{StreamID: streamID, FirstSeq: 1, LastSeq: 1, Items: []domain.EdgeReplayItem{{Seq: 1, Payload: validPayload}}},
			wantError: "generation must be positive",
		},
		{
			name:      "empty items",
			streamID:  streamID,
			gen:       1,
			batch:     &domain.EdgeReplayBatch{StreamID: streamID, FirstSeq: 1, LastSeq: 1, Items: nil},
			wantError: "event count must be 1..256",
		},
		{
			name:     "sequence gap",
			streamID: streamID,
			gen:      1,
			batch: &domain.EdgeReplayBatch{
				StreamID: streamID,
				FirstSeq: 1,
				LastSeq:  2,
				Items: []domain.EdgeReplayItem{
					{Seq: 1, Kind: "observation", ObservedAt: observedAt, Payload: validPayload},
					{Seq: 3, Kind: "observation", ObservedAt: observedAt, Payload: validPayload}, // gap
				},
			},
			wantError: "sequence gap",
		},
		{
			name:     "oversized payload",
			streamID: streamID,
			gen:      1,
			batch: &domain.EdgeReplayBatch{
				StreamID: streamID,
				FirstSeq: 1,
				LastSeq:  1,
				Items: []domain.EdgeReplayItem{
					{Seq: 1, Kind: "observation", ObservedAt: observedAt, Payload: bytes.Repeat([]byte("a"), (64<<10)+1)},
				},
			},
			wantError: "payload invalid size",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildReplayBatchFrame(tc.streamID, tc.gen, tc.batch)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestEdgeReplayPump_ReadinessGating(t *testing.T) {
	pump := newEdgeReplayPump(nil, nil, uuid.NewString(), uuid.NewString(), uuid.NewString(), 1)
	if pump.isHubReady() {
		t.Fatal("expected hub to not be ready initially")
	}

	// Hub health not ready
	pump.updateHubHealth(Health{Role: "hub", Ready: false, DBWritable: true, Errors: []string{}})
	if pump.isHubReady() {
		t.Fatal("expected hub to not be ready when Ready=false")
	}

	// Hub health DB not writable
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: false, Errors: []string{}})
	if pump.isHubReady() {
		t.Fatal("expected hub to not be ready when DBWritable=false")
	}

	// Hub health has ingest_unavailable
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, Errors: []string{"ingest_unavailable"}})
	if pump.isHubReady() {
		t.Fatal("expected hub to not be ready when ingest_unavailable present")
	}

	// Hub health role mismatch
	pump.updateHubHealth(Health{Role: "probe", Ready: true, DBWritable: true, Errors: []string{}})
	if pump.isHubReady() {
		t.Fatal("expected hub to not be ready when Role=probe")
	}

	// Fully ready hub health
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, Errors: []string{}})
	if !pump.isHubReady() {
		t.Fatal("expected hub to be ready")
	}
}

func TestEdgeReplayPump_ACKValidation_Table(t *testing.T) {
	streamID := uuid.NewString()
	hubID := uuid.NewString()
	probeID := uuid.NewString()
	generation := Decimal(1)

	tests := []struct {
		name      string
		ackGen    Decimal
		ack       TelemetryACK
		wantError string
	}{
		{
			name:   "mismatched generation",
			ackGen: 2,
			ack: TelemetryACK{
				StreamID:      streamID,
				CommittedSeq:  2,
				AcceptedCount: 2,
			},
			wantError: "ack generation mismatch",
		},
		{
			name:   "mismatched stream",
			ackGen: 1,
			ack: TelemetryACK{
				StreamID:      uuid.NewString(),
				CommittedSeq:  2,
				AcceptedCount: 2,
			},
			wantError: "ack stream mismatch",
		},
		{
			name:   "committed_seq does not cover full batch",
			ackGen: 1,
			ack: TelemetryACK{
				StreamID:      streamID,
				CommittedSeq:  1, // batch ends at 2
				AcceptedCount: 1,
			},
			wantError: "ack committed_seq mismatch",
		},
		{
			name:   "outcomes count mismatch",
			ackGen: 1,
			ack: TelemetryACK{
				StreamID:       streamID,
				CommittedSeq:   2,
				AcceptedCount:  1, // expected 2
				DuplicateCount: 0,
			},
			wantError: "ack outcomes count mismatch",
		},
		{
			name:   "rejected seq outside batch",
			ackGen: 1,
			ack: TelemetryACK{
				StreamID:      streamID,
				CommittedSeq:  2,
				AcceptedCount: 1,
				Rejected: []RejectedSequence{
					{Seq: 99, Code: "invalid"},
				},
			},
			wantError: "rejected seq 99 outside inflight batch",
		},
		{
			name:   "duplicate rejected seq",
			ackGen: 1,
			ack: TelemetryACK{
				StreamID:      streamID,
				CommittedSeq:  2,
				AcceptedCount: 0,
				Rejected: []RejectedSequence{
					{Seq: 1, Code: "invalid_1"},
					{Seq: 1, Code: "invalid_2"},
				},
			},
			wantError: "duplicate rejected seq in ack",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeReplayRepo{}
			pump := newEdgeReplayPump(repo, nil, hubID, probeID, streamID, generation)
			inflight := &inflightBatch{sent: true,
				streamID:  streamID,
				firstSeq:  1,
				lastSeq:   2,
				itemCount: 2,
			}
			pump.inflight = inflight

			// Exercise the actual production method processEvent!
			ev := replayEvent{
				kind:       "telemetry.ack",
				generation: tc.ackGen,
				ack:        tc.ack,
			}
			err := pump.processEvent(context.Background(), inflight, ev, nil)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantError)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("expected error containing %q, got %v", tc.wantError, err)
			}
		})
	}
}

func TestEdgeReplayPump_DuplicateACK_Handling(t *testing.T) {
	streamID := uuid.NewString()
	hubID := uuid.NewString()
	probeID := uuid.NewString()
	generation := Decimal(1)

	repo := &fakeReplayRepo{}
	pump := newEdgeReplayPump(repo, nil, hubID, probeID, streamID, generation)

	// Simulate previously committed batch
	pump.lastCompletedBatch = &completedBatchInfo{
		streamID:     streamID,
		committedSeq: 10,
		totalEvents:  5,
		rejections:   nil,
	}

	// 1. Proven duplicate ACK with changed accepted vs duplicate counts (retransmission)
	// Original was 5 accepted, retransmission reports 0 accepted, 5 duplicate.
	provenDup := TelemetryACK{
		StreamID:       streamID,
		CommittedSeq:   10,
		AcceptedCount:  0,
		DuplicateCount: 5,
		Rejected:       nil,
	}
	if !pump.isProvenDuplicateACK(provenDup, generation) {
		t.Fatal("expected proven duplicate ACK with changed counts to match")
	}

	// Calling processEvent with proven duplicate returns nil and does NOT call commitACK
	ev := replayEvent{kind: "telemetry.ack", generation: generation, ack: provenDup}
	if err := pump.processEvent(context.Background(), nil, ev, nil); err != nil {
		t.Fatalf("processEvent on proven duplicate returned error: %v", err)
	}
	if repo.commitACKCalls != 0 {
		t.Fatalf("proven duplicate re-pruned outbox: commitACKCalls = %d", repo.commitACKCalls)
	}

	// 2. Mismatched duplicate ACK (e.g. different total count)
	mismatchedDup := TelemetryACK{
		StreamID:       streamID,
		CommittedSeq:   10,
		AcceptedCount:  3,
		DuplicateCount: 1, // total 4 != 5
		Rejected:       nil,
	}
	if pump.isProvenDuplicateACK(mismatchedDup, generation) {
		t.Fatal("expected mismatched duplicate ACK to not match")
	}
	evMismatched := replayEvent{kind: "telemetry.ack", generation: generation, ack: mismatchedDup}
	if err := pump.processEvent(context.Background(), nil, evMismatched, nil); err == nil {
		t.Fatal("expected processEvent on mismatched duplicate to fail")
	}
}

func TestEdgeReplayPump_RetryCursorValidation(t *testing.T) {
	streamID := uuid.NewString()
	hubID := uuid.NewString()
	probeID := uuid.NewString()
	generation := Decimal(1)

	repo := &fakeReplayRepo{}
	pump := newEdgeReplayPump(repo, nil, hubID, probeID, streamID, generation)

	inflight := &inflightBatch{sent: true,
		streamID:  streamID,
		firstSeq:  5,
		lastSeq:   10,
		itemCount: 6,
	}

	// 1. Retry with committed_seq above sent last
	evOver := replayEvent{
		kind:       "telemetry.retry",
		generation: generation,
		retry: TelemetryRetry{
			StreamID:     streamID,
			CommittedSeq: 11, // > 10
			RetryAfterMS: 100,
		},
	}
	if err := pump.processEvent(context.Background(), inflight, evOver, nil); err == nil {
		t.Fatal("expected retry with committed_seq > inflight.lastSeq to fail")
	}

	// 2. A durable duplicate prefix after lost ACK never advances the local cursor.
	evAdvance := replayEvent{
		kind:       "telemetry.retry",
		generation: generation,
		retry: TelemetryRetry{
			StreamID:     streamID,
			CommittedSeq: 6, // > 4
			RetryAfterMS: 100,
		},
	}
	if err := pump.processEvent(context.Background(), inflight, evAdvance, nil); err != nil {
		t.Fatal("valid duplicate prefix rejected", err)
	}
	if repo.commitACKCalls != 0 {
		t.Fatal("retry pruned data")
	}

	// 3. Valid retry with committed_seq = 4 (unchanged known cursor) and large RetryAfterMS
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	evValid := replayEvent{
		kind:       "telemetry.retry",
		generation: generation,
		retry: TelemetryRetry{
			StreamID:     streamID,
			CommittedSeq: 4,
			RetryAfterMS: 50, // will be clamped to 100ms
		},
	}
	if err := pump.processEvent(ctx, inflight, evValid, nil); err != nil && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected valid retry to succeed or time out, got %v", err)
	}
}

func TestEdgeReplayPump_WelcomeAndHealthNeverPrune(t *testing.T) {
	repo := &fakeReplayRepo{}
	pump := newEdgeReplayPump(repo, nil, uuid.NewString(), uuid.NewString(), uuid.NewString(), 1)

	// Update health
	pump.updateHubHealth(Health{Role: "hub", Ready: true, DBWritable: true, Errors: []string{}})

	// Verify CommitReplayACK was NEVER called
	repo.mu.Lock()
	calls := repo.commitACKCalls
	repo.mu.Unlock()
	if calls != 0 {
		t.Fatalf("expected 0 CommitReplayACK calls from health, got %d", calls)
	}
}

func TestEdgeReplayPump_MicrosecondTimestampComparison(t *testing.T) {
	// Nanosecond timestamp in payload vs microsecond integer in store
	at := time.Date(2026, 9, 20, 12, 0, 0, 123456789, time.UTC)
	itemObservedAt := time.UnixMicro(at.UnixMicro()).UTC() // microsecond precision

	payload := []byte(fmt.Sprintf(`{
  "seq": "1",
  "kind": "observation",
  "observed_at": "%s",
  "data": {
    "monitor_id": 1,
    "assignment_generation": "1",
    "config_revision": "1",
    "status": "UP",
    "raw_status": "UP",
    "down_count": 0,
    "ping": 10,
    "duration_ms": 15,
    "message": "all good",
    "important": false,
    "conditions": [],
    "tls": null
  }
}`, at.Format(time.RFC3339Nano)))

	event, err := decodeTelemetryEvent(payload)
	if err != nil {
		t.Fatalf("decodeTelemetryEvent failed: %v", err)
	}

	// Direct comparison with Equal fails due to nanosecond difference:
	// time.Time(event.ObservedAt).Equal(itemObservedAt) is FALSE!
	if time.Time(event.ObservedAt).Equal(itemObservedAt) {
		t.Fatal("expected direct nanosecond vs microsecond comparison to not be equal")
	}

	// Truncated microsecond comparison succeeds:
	if time.Time(event.ObservedAt).UTC().UnixMicro() != itemObservedAt.UTC().UnixMicro() {
		t.Fatal("expected microsecond comparison to match")
	}
}

func TestEdgeRuntime_RealTLS_ReplayBatchAndACK(t *testing.T) {
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

	// Open raw SQLite connection to insert telemetry outbox rows and check tables directly
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "edge.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	enrollment := services.NewEdgeEnrollmentService(store)
	at := time.Now().UTC().Truncate(time.Microsecond)
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

	// Insert telemetry outbox rows directly into edge.db
	sampleObs1 := bytes.ReplaceAll(sampleObservationJSON(1, 101, ""), []byte("2026-09-20T12:00:00Z"), []byte(at.Format(time.RFC3339Nano)))
	sampleObs2 := bytes.ReplaceAll(sampleObservationJSON(2, 101, ""), []byte("2026-09-20T12:00:00Z"), []byte(at.Format(time.RFC3339Nano)))
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_identity SET last_created_seq=2 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO edge_telemetry_outbox(seq,kind,observed_at,payload) VALUES(?,?,?,?),(?,?,?,?)",
		1, "observation", at.UnixMicro(), sampleObs1,
		2, "observation", at.UnixMicro(), sampleObs2,
	); err != nil {
		t.Fatal(err)
	}

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

	// Configure Replay Repository on EdgeRuntime
	runtime.SetReplayRepository(store)

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

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:   client,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + runtimeToken}},
		Subprotocols: []string{"phoenix.probe.v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()

	// 1. Read hello
	_, helloData, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	helloEnv, _, err := DecodeHello(helloData)
	if err != nil || helloEnv.Type != "hello" {
		t.Fatalf("expected hello, got %v (%+v)", err, helloEnv)
	}

	// 2. Send welcome with generation 1
	welcome := Welcome{
		SessionIdentity:       SessionIdentity{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID},
		SelectedProtocol:      1,
		ConnectionGeneration:  1,
		DesiredConfigRevision: 1,
		HeartbeatSeconds:      HeartbeatSeconds,
		MaxFrameBytes:         MaxFrameBytes,
		MaxBatchEvents:        MaxBatchEvents,
		MaxBatchBytes:         MaxBatchBytes,
		HubTime:               Timestamp(time.Now().UTC()),
	}
	welcomeFrame, err := encodeFrame("welcome", 1, welcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, welcomeFrame); err != nil {
		t.Fatal(err)
	}

	// 3. Read initial health from probe
	_, healthData, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	healthEnv, _, err := DecodeHealth(healthData)
	if err != nil || healthEnv.Type != "health" {
		t.Fatalf("expected health from probe, got %v (%+v)", err, healthEnv)
	}

	// 4. Send hub health indicating readiness
	hubHealth := Health{
		Role:       "hub",
		Ready:      true,
		DBWritable: true,
		ClockTime:  Timestamp(time.Now().UTC()),
		Errors:     []string{},
	}
	hubHealthFrame, err := encodeFrame("health", 1, hubHealth)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hubHealthFrame); err != nil {
		t.Fatal(err)
	}

	// 5. Read telemetry.batch sent by replay pump
	var batchEnv Envelope
	var batchData TelemetryBatch
	for {
		_, frameData, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("failed reading replay batch: %v", err)
		}
		env, err := DecodeEnvelope(frameData)
		if err != nil {
			t.Fatalf("DecodeEnvelope failed: %v", err)
		}
		if env.Type == "telemetry.batch" {
			_, batchData, err = DecodeTelemetryBatch(frameData)
			if err != nil {
				t.Fatalf("DecodeTelemetryBatch failed: %v", err)
			}
			batchEnv = env
			break
		}
	}

	if batchEnv.ConnectionGeneration != 1 {
		t.Fatalf("expected generation 1, got %d", batchEnv.ConnectionGeneration)
	}
	if batchData.FirstSeq != 1 || batchData.LastSeq != 2 || len(batchData.Events) != 2 {
		t.Fatalf("unexpected batch data: %+v", batchData)
	}

	// The hub committed this prefix but its ACK was lost. Reconnect with the
	// advanced hub cursor and a new durable fence; welcome itself must not prune.
	_ = conn.CloseNow()
	if local, err := store.ReadIdentity(ctx); err != nil || local.CommittedSeq != 0 {
		t.Fatal("socket write/prior welcome pruned telemetry", err)
	}
	conn, _, err = websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + runtimeToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
	if err != nil {
		t.Fatal(err)
	}
	_, helloData, err = conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, hello, err := DecodeHello(helloData); err != nil || hello.FirstRetainedSeq != 1 {
		t.Fatal("lost ACK discarded local prefix", err)
	}
	welcome.ConnectionGeneration = 2
	welcome.CommittedSeq = 2
	welcomeFrame, err = encodeFrame("welcome", 2, welcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, welcomeFrame); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if local, err := store.ReadIdentity(ctx); err != nil || local.CommittedSeq != 0 {
		t.Fatal("reconnect welcome pruned telemetry", err)
	}
	hubHealthFrame, err = encodeFrame("health", 2, hubHealth)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, hubHealthFrame); err != nil {
		t.Fatal(err)
	}
	_, replayed, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if env, batch, err := DecodeTelemetryBatch(replayed); err != nil || env.ConnectionGeneration != 2 || batch.FirstSeq != 1 || batch.LastSeq != 2 {
		t.Fatal("lost ACK did not replay retained prefix", err)
	}
	if !bytes.Contains(replayed, sampleObs1) || !bytes.Contains(replayed, sampleObs2) {
		t.Fatal("reconnect changed exact stored bytes")
	}

	// 6. A durable duplicate receipt is the only authority to prune.
	ack := TelemetryACK{
		StreamID:       id.StreamID,
		CommittedSeq:   2,
		AcceptedCount:  0,
		DuplicateCount: 2,
		Rejected:       []RejectedSequence{},
	}
	ackFrame, err := encodeFrame("telemetry.ack", 2, ack)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, ackFrame); err != nil {
		t.Fatal(err)
	}

	// Wait briefly for edge to commit ACK
	time.Sleep(200 * time.Millisecond)

	// Verify store committed_seq was advanced to 2
	identity, err := store.ReadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if identity.CommittedSeq != 2 {
		t.Fatalf("expected committed_seq 2, got %d", identity.CommittedSeq)
	}

	// Verify outbox rows 1 and 2 were pruned from edge_telemetry_outbox
	var rowCount int64
	if err := db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM edge_telemetry_outbox").Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Fatalf("expected 0 rows in edge_telemetry_outbox after ACK commit, got %d", rowCount)
	}

	// Close cleanly
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEdgeReplayPump_WelcomeCursorBelowCommittedSeq(t *testing.T) {
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

	runtime, err := NewEdgeRuntime(func(ctx context.Context) (domain.EdgeIdentity, int64, error) {
		return domain.EdgeIdentity{
			HubID:                hubID,
			Fingerprint:          id.Fingerprint,
			ProbeID:              id.ProbeID,
			StreamID:             id.StreamID,
			ConnectionGeneration: 1,
			CommittedSeq:         5, // edge committed cursor is 5
			LastCreatedSeq:       10,
		}, 5, nil
	}, store, &services.EdgeConfigService{}, EdgeRuntimeConfig{AgentVersion: "test", Capabilities: []string{"snapshot.v1"}}, func(context.Context) (Health, error) {
		return Health{}, nil
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

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{
		HTTPClient:   client,
		HTTPHeader:   http.Header{"Authorization": {"Bearer " + runtimeToken}},
		Subprotocols: []string{"phoenix.probe.v1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Read hello
	_, helloData, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	helloEnv, _, err := DecodeHello(helloData)
	if err != nil || helloEnv.Type != "hello" {
		t.Fatalf("expected hello, got %v (%+v)", err, helloEnv)
	}

	// Send welcome with CommittedSeq = 2 (< 5)
	welcome := Welcome{
		SessionIdentity:       SessionIdentity{HubID: hubID, ProbeID: id.ProbeID, StreamID: id.StreamID},
		SelectedProtocol:      1,
		ConnectionGeneration:  2,
		CommittedSeq:          2, // below edge's committed sequence of 5
		DesiredConfigRevision: 1,
		HeartbeatSeconds:      HeartbeatSeconds,
		MaxFrameBytes:         MaxFrameBytes,
		MaxBatchEvents:        MaxBatchEvents,
		MaxBatchBytes:         MaxBatchBytes,
		HubTime:               Timestamp(time.Now().UTC()),
	}
	welcomeFrame, err := encodeFrame("welcome", 2, welcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, welcomeFrame); err != nil {
		t.Fatal(err)
	}

	// Read next frame - connection should be closed by edge runtime
	_, _, err = conn.Read(ctx)
	if err == nil {
		t.Fatal("expected connection to be closed after welcome with cursor below edge committed_seq")
	}
}
