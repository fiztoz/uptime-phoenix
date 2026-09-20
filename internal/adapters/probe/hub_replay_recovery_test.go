package probe

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type controlledHubPeerSetup struct {
	id        *RuntimeIdentity
	server    *httptest.Server
	transport *HubTransport
	input     domain.ProbeSessionInput
	snapshot  ConfigSnapshot
	connCh    chan *websocket.Conn
}

func setupControlledHubPeer(t *testing.T) *controlledHubPeerSetup {
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

	connCh := make(chan *websocket.Conn, 1)
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x2a}, 32))

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/probe/v1" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			Subprotocols:    []string{"phoenix.probe.v1"},
			CompressionMode: websocket.CompressionDisabled,
		})
		if err != nil {
			return
		}
		connCh <- c
		<-r.Context().Done()
	})

	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{id.Certificate},
	}
	server.StartTLS()
	t.Cleanup(server.Close)

	hubID := uuid.NewString()
	endpoint := "wss" + strings.TrimPrefix(server.URL, "https") + "/ws/probe/v1"
	m := domain.ProbeCredentialMetadata{
		HubID:             hubID,
		ProbeID:           id.ProbeID,
		StreamID:          id.StreamID,
		EnrollmentID:      uuid.NewString(),
		CredentialVersion: 1,
		Endpoint:          endpoint,
		Fingerprint:       id.Fingerprint,
	}

	snapshot := m2Config(t)
	snapshot.HubID, snapshot.ProbeID = m.HubID, m.ProbeID
	document := configBytes(t, snapshot)

	in := domain.ProbeSessionInput{
		Connection:     m,
		Token:          token,
		Generation:     1,
		ConfigDocument: document,
	}

	transport := NewHubTransport(EndpointPolicy{
		AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
	})

	return &controlledHubPeerSetup{
		id:        id,
		server:    server,
		transport: transport,
		input:     in,
		snapshot:  snapshot,
		connCh:    connCh,
	}
}

func performHandshakeAndConfig(ctx context.Context, t *testing.T, conn *websocket.Conn, s *controlledHubPeerSetup) {
	t.Helper()

	hello := Hello{
		SessionIdentity: SessionIdentity{
			HubID:    s.input.Connection.HubID,
			ProbeID:  s.input.Connection.ProbeID,
			StreamID: s.input.Connection.StreamID,
		},
		AgentVersion:     "test",
		ProtocolMin:      1,
		ProtocolMax:      1,
		ConfigRevision:   s.snapshot.Revision,
		FirstRetainedSeq: 1,
		LastCreatedSeq:   100,
		Capabilities:     []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"},
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
	env, welcome, err := DecodeWelcome(welcomeData)
	if err != nil {
		t.Fatalf("decode welcome: %v", err)
	}
	if env.Type != "welcome" || welcome.ConnectionGeneration != Decimal(s.input.Generation) {
		t.Fatalf("unexpected welcome: %+v", env)
	}

	trueVal := true
	zero := int64(0)
	h := Health{
		Role:             "probe",
		Ready:            true,
		DBWritable:       true,
		SchedulerHealthy: &trueVal,
		ConfigRevision:   welcome.DesiredConfigRevision,
		CommittedSeq:     0,
		QueueBytes:       &zero,
		OldestQueuedAt:   nil,
		ClockTime:        Timestamp(time.Now().UTC()),
		Errors:           []string{},
	}
	healthFrame, err := encodeFrame("health", Decimal(s.input.Generation), h)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, healthFrame); err != nil {
		t.Fatal(err)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("reading config frames: %v", err)
		}
		env, err := DecodeEnvelope(data)
		if err != nil {
			t.Fatalf("decode envelope: %v", err)
		}
		if env.Type == "health" {
			continue
		}
		if env.Type == "config.begin" || env.Type == "config.chunk" {
			continue
		}
		if env.Type == "config.commit" {
			_, commit, err := DecodeConfigCommit(data)
			if err != nil {
				t.Fatalf("decode config commit: %v", err)
			}
			appliedFrame, err := encodeFrame("config.applied", Decimal(s.input.Generation), ConfigApplied{
				ConfigTransferIdentity: commit.ConfigTransferIdentity,
				SHA256:                 commit.SHA256,
				AppliedAt:              Timestamp(time.Now().UTC()),
				AssignmentCount:        len(s.snapshot.Assignments),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := conn.Write(ctx, websocket.MessageText, appliedFrame); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
}

func readReplayResponse(ctx context.Context, conn *websocket.Conn) (Envelope, []byte, error) {
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			return Envelope{}, nil, err
		}
		if msgType != websocket.MessageText {
			return Envelope{}, nil, errors.New("expected text frame")
		}
		env, err := DecodeEnvelope(data)
		if err != nil {
			return Envelope{}, nil, err
		}
		if env.Type == "health" {
			continue
		}
		return env, data, nil
	}
}

func TestHubTransport_ReplayGap_CommittedEmitsZeroCountACK(t *testing.T) {
	s := setupControlledHubPeer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	fromTime := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	throughTime := time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC)
	affectedIDs := []int64{101, 102, 103}

	var (
		ingestCalled  atomic.Bool
		receivedBatch domain.ProbeReplayBatch
	)

	ingest := func(ctx context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
		receivedBatch = batch
		ingestCalled.Store(true)
		return &domain.ProbeReplayResult{
			StreamID:       batch.StreamID,
			CommittedSeq:   batch.LastSeq,
			AcceptedCount:  0,
			DuplicateCount: 0,
			Rejected:       nil,
		}, ctx.Err()
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, ingest)
	}()

	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case err := <-runErrCh:
		t.Fatalf("Run exited before connection: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for peer connection")
	}
	defer func() { _ = conn.CloseNow() }()

	performHandshakeAndConfig(ctx, t, conn, s)

	gap := TelemetryGap{
		StreamID:           s.input.Connection.StreamID,
		FromSeq:            10,
		ThroughSeq:         20,
		Reason:             "retention_age",
		ObservedFrom:       Timestamp(fromTime),
		ObservedThrough:    Timestamp(throughTime),
		AffectedMonitorIDs: affectedIDs,
	}
	gapFrame, err := encodeFrame("telemetry.gap", Decimal(s.input.Generation), gap)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, gapFrame); err != nil {
		t.Fatal(err)
	}

	env, ackData, err := readReplayResponse(ctx, conn)
	if err != nil {
		t.Fatalf("reading ack response: %v", err)
	}
	if env.Type != "telemetry.ack" {
		t.Fatalf("expected telemetry.ack, got %s", env.Type)
	}
	if env.ConnectionGeneration != Decimal(s.input.Generation) {
		t.Fatalf("expected generation %d, got %d", s.input.Generation, env.ConnectionGeneration)
	}
	_, ack, err := DecodeTelemetryACK(ackData)
	if err != nil {
		t.Fatalf("decode telemetry.ack: %v", err)
	}
	if ack.StreamID != s.input.Connection.StreamID {
		t.Fatalf("expected stream %s, got %s", s.input.Connection.StreamID, ack.StreamID)
	}
	if ack.CommittedSeq != 20 {
		t.Fatalf("expected committed_seq 20, got %d", ack.CommittedSeq)
	}
	if ack.AcceptedCount != 0 {
		t.Fatalf("expected 0 accepted_count, got %d", ack.AcceptedCount)
	}
	if ack.DuplicateCount != 0 {
		t.Fatalf("expected 0 duplicate_count, got %d", ack.DuplicateCount)
	}
	if len(ack.Rejected) != 0 {
		t.Fatalf("expected 0 rejected, got %d", len(ack.Rejected))
	}

	if !ingestCalled.Load() {
		t.Fatal("ingest callback was never invoked")
	}
	if receivedBatch.ProbeID != s.input.Connection.ProbeID {
		t.Fatalf("expected probe ID %s, got %s", s.input.Connection.ProbeID, receivedBatch.ProbeID)
	}
	if receivedBatch.StreamID != s.input.Connection.StreamID {
		t.Fatalf("expected stream ID %s, got %s", s.input.Connection.StreamID, receivedBatch.StreamID)
	}
	if receivedBatch.FirstSeq != 10 || receivedBatch.LastSeq != 20 {
		t.Fatalf("expected sequence range [10, 20], got [%d, %d]", receivedBatch.FirstSeq, receivedBatch.LastSeq)
	}
	if len(receivedBatch.Events) != 0 {
		t.Fatalf("expected 0 events in gap batch, got %d", len(receivedBatch.Events))
	}
	if receivedBatch.Gap == nil {
		t.Fatal("received batch Gap is nil")
	}
	if receivedBatch.Gap.StreamID != s.input.Connection.StreamID {
		t.Fatalf("expected gap stream ID %s, got %s", s.input.Connection.StreamID, receivedBatch.Gap.StreamID)
	}
	if receivedBatch.Gap.FromSeq != 10 || receivedBatch.Gap.ThroughSeq != 20 {
		t.Fatalf("expected gap range [10, 20], got [%d, %d]", receivedBatch.Gap.FromSeq, receivedBatch.Gap.ThroughSeq)
	}
	if receivedBatch.Gap.Reason != "retention_age" {
		t.Fatalf("expected reason retention_age, got %s", receivedBatch.Gap.Reason)
	}
	if !receivedBatch.Gap.ObservedFrom.Equal(fromTime) {
		t.Fatalf("expected observed_from %v, got %v", fromTime, receivedBatch.Gap.ObservedFrom)
	}
	if !receivedBatch.Gap.ObservedThrough.Equal(throughTime) {
		t.Fatalf("expected observed_through %v, got %v", throughTime, receivedBatch.Gap.ObservedThrough)
	}
	if !slices.Equal(receivedBatch.Gap.AffectedMonitorIDs, affectedIDs) {
		t.Fatalf("expected affected monitor IDs %v, got %v", affectedIDs, receivedBatch.Gap.AffectedMonitorIDs)
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "connection ended") {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not join after cancellation")
	}
}

func TestHubTransport_ReplayRetry_PeerReceivesRetryAndRecoversOnSameSession(t *testing.T) {
	s := setupControlledHubPeer(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var attempt atomic.Int32
	ingest := func(_ context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
		n := attempt.Add(1)
		if n == 1 {
			// First attempt returns ErrReplayRetry with durable bounded cursor (FirstSeq - 1).
			return &domain.ProbeReplayResult{
				StreamID:     batch.StreamID,
				CommittedSeq: batch.FirstSeq - 1, // 9
			}, domain.ErrReplayRetry
		}
		// Second attempt succeeds on same session.
		return &domain.ProbeReplayResult{
			StreamID:       batch.StreamID,
			CommittedSeq:   batch.LastSeq, // 15
			AcceptedCount:  0,
			DuplicateCount: 0,
		}, nil
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, ingest)
	}()

	var conn *websocket.Conn
	select {
	case conn = <-s.connCh:
	case err := <-runErrCh:
		t.Fatalf("Run exited before connection: %v", err)
	case <-ctx.Done():
		t.Fatal("timed out waiting for peer connection")
	}
	defer func() { _ = conn.CloseNow() }()

	performHandshakeAndConfig(ctx, t, conn, s)

	gap := TelemetryGap{
		StreamID:           s.input.Connection.StreamID,
		FromSeq:            10,
		ThroughSeq:         15,
		Reason:             "retention_bytes",
		ObservedFrom:       Timestamp(time.Now().UTC().Add(-time.Hour)),
		ObservedThrough:    Timestamp(time.Now().UTC()),
		AffectedMonitorIDs: []int64{101},
	}
	gapFrame, err := encodeFrame("telemetry.gap", Decimal(s.input.Generation), gap)
	if err != nil {
		t.Fatal(err)
	}

	// First attempt: peer sends gap frame.
	if err := conn.Write(ctx, websocket.MessageText, gapFrame); err != nil {
		t.Fatal(err)
	}

	// Peer must see telemetry.retry rather than telemetry.ack.
	env, retryData, err := readReplayResponse(ctx, conn)
	if err != nil {
		t.Fatalf("reading retry response: %v", err)
	}
	if env.Type != "telemetry.retry" {
		t.Fatalf("expected telemetry.retry, got %s", env.Type)
	}
	if env.ConnectionGeneration != Decimal(s.input.Generation) {
		t.Fatalf("expected generation %d, got %d", s.input.Generation, env.ConnectionGeneration)
	}
	_, retry, err := DecodeTelemetryRetry(retryData)
	if err != nil {
		t.Fatalf("decode telemetry.retry: %v", err)
	}
	if retry.StreamID != s.input.Connection.StreamID {
		t.Fatalf("expected stream %s, got %s", s.input.Connection.StreamID, retry.StreamID)
	}
	if retry.CommittedSeq != 9 {
		t.Fatalf("expected retry committed_seq 9, got %d", retry.CommittedSeq)
	}
	if retry.RetryAfterMS != 1000 {
		t.Fatalf("expected retry_after_ms 1000, got %d", retry.RetryAfterMS)
	}

	// Peer sends retry on the SAME session.
	if err := conn.Write(ctx, websocket.MessageText, gapFrame); err != nil {
		t.Fatalf("sending retry on same session: %v", err)
	}

	// After retry, peer receives telemetry.ack.
	env, ackData, err := readReplayResponse(ctx, conn)
	if err != nil {
		t.Fatalf("reading ack response after retry: %v", err)
	}
	if env.Type != "telemetry.ack" {
		t.Fatalf("expected telemetry.ack, got %s", env.Type)
	}
	_, ack, err := DecodeTelemetryACK(ackData)
	if err != nil {
		t.Fatalf("decode telemetry.ack: %v", err)
	}
	if ack.StreamID != s.input.Connection.StreamID {
		t.Fatalf("expected stream %s, got %s", s.input.Connection.StreamID, ack.StreamID)
	}
	if ack.CommittedSeq != 15 {
		t.Fatalf("expected committed_seq 15, got %d", ack.CommittedSeq)
	}

	if attempt.Load() != 2 {
		t.Fatalf("expected exactly 2 ingest attempts, got %d", attempt.Load())
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "connection ended") {
			t.Fatalf("unexpected Run error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("transport did not join after cancellation")
	}
}

func TestHubTransport_ReplayConflictAuthority_ClosesWithoutRetryOrACK(t *testing.T) {
	t.Run("NonRetryIngestError", func(t *testing.T) {
		s := setupControlledHubPeer(t)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		ingest := func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return nil, ports.ErrConflict
		}

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, ingest)
		}()

		var conn *websocket.Conn
		select {
		case conn = <-s.connCh:
		case err := <-runErrCh:
			t.Fatalf("Run exited before connection: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for peer connection")
		}
		defer func() { _ = conn.CloseNow() }()

		performHandshakeAndConfig(ctx, t, conn, s)

		gap := TelemetryGap{
			StreamID:           s.input.Connection.StreamID,
			FromSeq:            10,
			ThroughSeq:         20,
			Reason:             "retention_age",
			ObservedFrom:       Timestamp(time.Now().UTC().Add(-time.Hour)),
			ObservedThrough:    Timestamp(time.Now().UTC()),
			AffectedMonitorIDs: []int64{101},
		}
		gapFrame, err := encodeFrame("telemetry.gap", Decimal(s.input.Generation), gap)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, gapFrame); err != nil {
			t.Fatal(err)
		}

		// Conflict error must close: reading from peer must fail, no retry or ACK.
		env, data, err := readReplayResponse(ctx, conn)
		if err == nil {
			t.Fatalf("expected connection close, but received frame %s: %s", env.Type, string(data))
		}

		select {
		case runErr := <-runErrCh:
			if runErr == nil {
				t.Fatal("expected HubTransport.Run to return error on ingest conflict")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for HubTransport.Run to exit")
		}
	})

	t.Run("UnboundedCursorOnRetryError", func(t *testing.T) {
		s := setupControlledHubPeer(t)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		ingest := func(_ context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			// CommittedSeq 5 is < batch.FirstSeq - 1 (9), which is an unbounded/invalid cursor.
			return &domain.ProbeReplayResult{
				StreamID:     batch.StreamID,
				CommittedSeq: batch.FirstSeq - 5,
			}, domain.ErrReplayRetry
		}

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- s.transport.Run(ctx, s.input, func(context.Context) error { return nil }, func(context.Context, domain.ProbeActiveConfig) error { return nil }, ingest)
		}()

		var conn *websocket.Conn
		select {
		case conn = <-s.connCh:
		case err := <-runErrCh:
			t.Fatalf("Run exited before connection: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for peer connection")
		}
		defer func() { _ = conn.CloseNow() }()

		performHandshakeAndConfig(ctx, t, conn, s)

		gap := TelemetryGap{
			StreamID:           s.input.Connection.StreamID,
			FromSeq:            10,
			ThroughSeq:         20,
			Reason:             "retention_age",
			ObservedFrom:       Timestamp(time.Now().UTC().Add(-time.Hour)),
			ObservedThrough:    Timestamp(time.Now().UTC()),
			AffectedMonitorIDs: []int64{101},
		}
		gapFrame, err := encodeFrame("telemetry.gap", Decimal(s.input.Generation), gap)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, gapFrame); err != nil {
			t.Fatal(err)
		}

		// Unbounded cursor must not produce telemetry.retry; must close session.
		env, data, err := readReplayResponse(ctx, conn)
		if err == nil {
			t.Fatalf("expected connection close, but received frame %s: %s", env.Type, string(data))
		}

		select {
		case runErr := <-runErrCh:
			if runErr == nil {
				t.Fatal("expected HubTransport.Run to return error on unbounded retry cursor")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for HubTransport.Run to exit")
		}
	})

	t.Run("EstablishedAuthorityError", func(t *testing.T) {
		s := setupControlledHubPeer(t)

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()

		established := func(context.Context) error {
			return errors.New("authority unavailable: DB lease expired")
		}

		runErrCh := make(chan error, 1)
		go func() {
			runErrCh <- s.transport.Run(ctx, s.input, established, func(context.Context, domain.ProbeActiveConfig) error { return nil }, nil)
		}()

		var conn *websocket.Conn
		select {
		case conn = <-s.connCh:
		case err := <-runErrCh:
			t.Fatalf("Run exited before connection: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for peer connection")
		}
		defer func() { _ = conn.CloseNow() }()

		// Perform initial handshake.
		hello := Hello{
			SessionIdentity: SessionIdentity{
				HubID:    s.input.Connection.HubID,
				ProbeID:  s.input.Connection.ProbeID,
				StreamID: s.input.Connection.StreamID,
			},
			AgentVersion:     "test",
			ProtocolMin:      1,
			ProtocolMax:      1,
			ConfigRevision:   s.snapshot.Revision,
			FirstRetainedSeq: 1,
			LastCreatedSeq:   100,
			Capabilities:     []string{"snapshot.v1", "checker.http.v1", "notifier.webhook.v1"},
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
		if _, _, err := DecodeWelcome(welcomeData); err != nil {
			t.Fatalf("decode welcome: %v", err)
		}

		// Send probe health: Hub will invoke established(), which fails.
		trueVal := true
		zero := int64(0)
		h := Health{
			Role:             "probe",
			Ready:            true,
			DBWritable:       true,
			SchedulerHealthy: &trueVal,
			ConfigRevision:   s.snapshot.Revision,
			CommittedSeq:     0,
			QueueBytes:       &zero,
			OldestQueuedAt:   nil,
			ClockTime:        Timestamp(time.Now().UTC()),
			Errors:           []string{},
		}
		healthFrame, err := encodeFrame("health", Decimal(s.input.Generation), h)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.Write(ctx, websocket.MessageText, healthFrame); err != nil {
			t.Fatal(err)
		}

		// Config transfer is queued during handshake, before the health
		// callback. It may already be in flight when authority fails.
		// No successful health or replay receipt may escape that failure.
		for {
			_, data, err := conn.Read(ctx)
			if err != nil {
				break
			}
			env, err := DecodeEnvelope(data)
			if err != nil {
				t.Fatal(err)
			}
			switch env.Type {
			case "config.begin", "config.chunk", "config.commit":
			case "health":
				_, h, err := DecodeHealth(data)
				if err != nil || h.Ready {
					t.Fatalf("authority failure emitted ready health: %+v %v", h, err)
				}
			default:
				t.Fatalf("authority failure emitted %s", env.Type)
			}
		}

		select {
		case runErr := <-runErrCh:
			if runErr == nil {
				t.Fatal("expected HubTransport.Run to return error on established authority failure")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for HubTransport.Run to exit")
		}
	})
}
