package services

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type connectorConnections struct {
	ports.ProbeConnectionRepository
	value                 *domain.ProbeConnection
	prepareErr, cursorErr error
	activated             int
}

func (f *connectorConnections) GetConnection(context.Context, string) (*domain.ProbeConnection, error) {
	if f.value == nil {
		return nil, ports.ErrNotFound
	}
	c := *f.value
	return &c, nil
}
func (f *connectorConnections) PrepareConnection(_ context.Context, c domain.ProbeConnection) (*domain.ProbeConnection, error) {
	if f.prepareErr != nil {
		return nil, f.prepareErr
	}
	f.value = &c
	return &c, nil
}
func (f *connectorConnections) ActivateConnection(context.Context, string, string, int64, time.Time) error {
	f.activated++
	f.value.State = "active"
	return nil
}
func (f *connectorConnections) GetConnectionCursor(context.Context, string, string) (int64, error) {
	return 0, f.cursorErr
}

type connectorLeases struct {
	ports.ProbeConnectorLeaseRepository
	renewErr, connectedErr error
	released               atomic.Int64
	runtimeAcquired        atomic.Int64
	runtimeReleased        atomic.Int64
	runtimeRenewed         atomic.Int64
	runtimeRenewErr        error
	runtimeAcquireErr      error
	runtimeRenew           func(context.Context, domain.ProbeRuntimeLease) (domain.ProbeRuntimeLease, error)
	connected              atomic.Bool
}

func (f *connectorLeases) AcquireRuntime(_ context.Context, probeID, ownerID string) (domain.ProbeRuntimeLease, error) {
	f.runtimeAcquired.Add(1)
	if f.runtimeAcquireErr != nil {
		return domain.ProbeRuntimeLease{}, f.runtimeAcquireErr
	}
	return domain.ProbeRuntimeLease{ProbeID: probeID, OwnerID: ownerID, Epoch: 1, LeaseUntil: time.Now().UTC().Add(time.Minute)}, nil
}
func (f *connectorLeases) RenewRuntime(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeRuntimeLease, error) {
	f.runtimeRenewed.Add(1)
	if f.runtimeRenew != nil {
		return f.runtimeRenew(ctx, lease)
	}
	return lease, f.runtimeRenewErr
}
func (f *connectorLeases) ReleaseRuntime(_ context.Context, lease domain.ProbeRuntimeLease) error {
	f.runtimeReleased.Add(1)
	return nil
}
func (f *connectorLeases) AcquireRuntimeConnector(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeConnectorLease, error) {
	return f.AcquireConnector(ctx, lease.ProbeID, lease.OwnerID)
}

func connectorTestOnce(s *ProbeConnectorService, ctx context.Context, probeID string) error {
	return s.withRuntime(ctx, probeID, func(ctx context.Context, lease domain.ProbeRuntimeLease) error {
		_, err := s.connectOnce(ctx, lease)
		return err
	})
}

func (f *connectorLeases) AcquireConnector(_ context.Context, probeID, owner string) (domain.ProbeConnectorLease, error) {
	return domain.ProbeConnectorLease{ProbeID: probeID, OwnerID: owner, Generation: 7, LeaseUntil: time.Now().UTC().Add(time.Minute)}, nil
}
func (f *connectorLeases) RenewConnector(_ context.Context, lease domain.ProbeConnectorLease) (domain.ProbeConnectorLease, error) {
	return lease, f.renewErr
}
func (f *connectorLeases) ReleaseConnector(_ context.Context, lease domain.ProbeConnectorLease) error {
	f.released.Store(lease.Generation)
	return nil
}
func (f *connectorLeases) SetConnectorConnected(_ context.Context, _ domain.ProbeConnectorLease, connected bool) error {
	if f.connectedErr != nil {
		return f.connectedErr
	}
	f.connected.Store(connected)
	return nil
}

type connectorProtector struct{}

func (connectorProtector) SealCredential(context.Context, domain.ProbeCredentialMetadata, string) ([]byte, error) {
	return []byte("protected-credential"), nil
}
func (connectorProtector) OpenCredential(context.Context, domain.ProbeCredentialMetadata, []byte) (string, error) {
	return "private-runtime-token", nil
}

type connectorTransport struct {
	run     func(context.Context, domain.ProbeSessionInput, func(context.Context) error) error
	applied func(context.Context, func(context.Context, domain.ProbeActiveConfig) error) error
	enroll  func(context.Context, domain.ProbeCredentialMetadata, string, string) error
	ingest  func(context.Context, func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error
}

func (f connectorTransport) Run(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error, applied func(context.Context, domain.ProbeActiveConfig) error, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error {
	if f.ingest != nil {
		return f.ingest(ctx, ingest)
	}
	if f.applied != nil {
		return f.applied(ctx, applied)
	}
	return f.run(ctx, in, ready)
}
func (f connectorTransport) Enroll(ctx context.Context, m domain.ProbeCredentialMetadata, enrollment, token string) error {
	return f.enroll(ctx, m, enrollment, token)
}

func newConnectorTestService(t *testing.T, connections *connectorConnections, leases *connectorLeases, transport connectorTransport) *ProbeConnectorService {
	t.Helper()
	configs := NewProbeConfigService(&configServiceRepo{err: ports.ErrNotFound}, configServiceInspector{}, &configServiceProtector{})
	svc, err := NewProbeConnectorService(connections, leases, leases, connectorProtector{}, configs, transport,
		"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", func(int, time.Duration) time.Duration { return time.Second })
	if err != nil {
		t.Fatal(err)
	}
	return svc
}
func prepareConnectorTest(t *testing.T, s *ProbeConnectorService) {
	t.Helper()
	if _, err := s.Prepare(t.Context(), "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", "wss://edge.example/ws/probe/v1", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestProbeConnectorPreparedCredentialsSurviveLostEnrollmentReceipt(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	transport := connectorTransport{enroll: func(_ context.Context, m domain.ProbeCredentialMetadata, enrollment, token string) error {
		if connections.value == nil || connections.value.EnrollmentID != m.EnrollmentID || string(connections.value.ProtectedCredential) != "protected-credential" || enrollment != "one-time-token" || token != "private-runtime-token" {
			t.Fatal("network preceded durable protected preparation")
		}
		return errors.New("receipt lost")
	}}
	svc := newConnectorTestService(t, connections, leases, transport)
	prepareConnectorTest(t, svc)
	id := connections.value.EnrollmentID
	if svc.Enroll(t.Context(), connections.value.ProbeID, "one-time-token") == nil {
		t.Fatal("lost receipt reported success")
	}
	if connections.value.State != "prepared" || leases.runtimeAcquired.Load() != 0 {
		t.Fatal("failed enrollment destroyed recovery or competed for runtime ownership")
	}
	prepareConnectorTest(t, svc)
	if connections.value.EnrollmentID != id {
		t.Fatal("retry regenerated identity")
	}
	if connections.activated != 0 {
		t.Fatal("unverified enrollment marked active")
	}
}

func TestProbeConnectorRejectsMissingCursorBeforeNetwork(t *testing.T) {
	connections, leases := &connectorConnections{cursorErr: ports.ErrNotFound}, &connectorLeases{}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(context.Context, domain.ProbeSessionInput, func(context.Context) error) error {
		t.Fatal("unknown stream reached network")
		return nil
	}})
	prepareConnectorTest(t, svc)
	if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("invented cursor")
	}
	if leases.released.Load() != 7 {
		t.Fatal("early failure leaked lease")
	}
}

func TestProbeConnectorCannotActivateAfterFencedCallback(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{connectedErr: ports.ErrConflict}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error) error {
		if in.Generation != 7 {
			t.Fatal("wrong lease fence")
		}
		return ready(ctx)
	}})
	prepareConnectorTest(t, svc)
	if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("ignored stale callback")
	}
	if connections.activated != 0 || leases.released.Load() != 7 {
		t.Fatal("stale connection activated or released wrong fence")
	}
}

func TestProbeConnectorLeaseRenewalFailureCancelsSocket(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{renewErr: errors.New("database unavailable")}
	var canceled atomic.Bool
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error) error {
		if err := ready(ctx); err != nil {
			return err
		}
		<-ctx.Done()
		canceled.Store(true)
		return ctx.Err()
	}})
	prepareConnectorTest(t, svc)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := connectorTestOnce(svc, ctx, connections.value.ProbeID)
	if err == nil || ctx.Err() != nil || !canceled.Load() || time.Since(start) < 14*time.Second || leases.released.Load() != 7 || connections.activated != 1 {
		t.Fatal("lease renewal did not revoke the live socket and join its owner")
	}
}

type mockReplayService struct {
	calledBatch   domain.ProbeReplayBatch
	calledSession domain.ProbeReplaySession
	result        *domain.ProbeReplayResult
	err           error
}

func (m *mockReplayService) ProcessBatch(_ context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
	m.calledSession = session
	m.calledBatch = batch
	return m.result, m.err
}

func TestProbeConnectorReplayIngestWiring(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	mockReplay := &mockReplayService{
		result: &domain.ProbeReplayResult{
			StreamID:      "44444444-4444-4444-8444-444444444444",
			CommittedSeq:  10,
			AcceptedCount: 1,
		},
	}
	var receivedIngest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)
	transport := connectorTransport{
		ingest: func(_ context.Context, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error {
			receivedIngest = ingest
			return nil
		},
	}
	svc := newConnectorTestService(t, connections, leases, transport)
	svc.SetReplayIngest(mockReplay)
	prepareConnectorTest(t, svc)

	err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedIngest == nil {
		t.Fatal("expected ingest callback to be passed to transport.Run")
	}

	batch := domain.ProbeReplayBatch{
		ProbeID:  connections.value.ProbeID,
		StreamID: connections.value.StreamID,
		FirstSeq: 1,
		LastSeq:  1,
	}
	res, err := receivedIngest(t.Context(), batch)
	if err != nil {
		t.Fatalf("unexpected ingest error: %v", err)
	}
	if res.CommittedSeq != 10 {
		t.Fatalf("expected committedSeq 10, got %d", res.CommittedSeq)
	}
	if mockReplay.calledSession.HubID != "11111111-1111-4111-8111-111111111111" ||
		mockReplay.calledSession.ProbeID != connections.value.ProbeID ||
		mockReplay.calledSession.StreamID != connections.value.StreamID ||
		mockReplay.calledSession.ConnectionGeneration != 7 ||
		mockReplay.calledSession.OwnerID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("unexpected session captured in replay: %+v", mockReplay.calledSession)
	}
}

func TestProbeConnectorReplayIngestNilWhenUnconfigured(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	var receivedIngest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)
	transport := connectorTransport{
		ingest: func(_ context.Context, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error {
			receivedIngest = ingest
			return nil
		},
	}
	svc := newConnectorTestService(t, connections, leases, transport)
	prepareConnectorTest(t, svc)

	err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedIngest != nil {
		t.Fatal("expected nil ingest callback when SetReplayIngest is not called")
	}
}

func TestProbeConnectorRuntimeSurvivesReconnectBackoff(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(context.Context, domain.ProbeSessionInput, func(context.Context) error) error {
		calls++
		if leases.runtimeAcquired.Load() != 1 || leases.runtimeReleased.Load() != 0 {
			t.Error("connection attempts changed their runtime owner")
		}
		if calls == 2 {
			cancel()
		}
		return errors.New("connection lost")
	}})
	prepareConnectorTest(t, svc)
	svc.renewInterval = 20 * time.Millisecond
	svc.connectLoop(ctx, connections.value.ProbeID, nil)
	if calls != 2 || leases.runtimeAcquired.Load() != 1 || leases.runtimeReleased.Load() != 1 || leases.runtimeRenewed.Load() < 1 {
		t.Fatalf("ownership did not survive backoff: calls=%d acquired=%d released=%d renewed=%d", calls, leases.runtimeAcquired.Load(), leases.runtimeReleased.Load(), leases.runtimeRenewed.Load())
	}
}

func TestProbeConnectorRuntimeFailureCancelsAndJoinsSession(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{runtimeRenewErr: ports.ErrConflict}
	var joined atomic.Bool
	svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, _ domain.ProbeSessionInput, _ func(context.Context) error) error {
		<-ctx.Done()
		if leases.runtimeReleased.Load() != 0 {
			t.Error("runtime released before live session stopped")
		}
		joined.Store(true)
		return ctx.Err()
	}})
	prepareConnectorTest(t, svc)
	svc.renewInterval = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := connectorTestOnce(svc, ctx, connections.value.ProbeID)
	if !errors.Is(err, ports.ErrConflict) || ctx.Err() != nil || !joined.Load() || leases.runtimeReleased.Load() != 1 || leases.released.Load() != 7 {
		t.Fatalf("failed ownership retained work: %v joined=%v runtime_release=%d session_release=%d", err, joined.Load(), leases.runtimeReleased.Load(), leases.released.Load())
	}
}

func TestProbeConnectorRuntimeCompletionDoesNotBecomeCanceled(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	started := make(chan struct{})
	leases.runtimeRenew = func(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeRuntimeLease, error) {
		close(started)
		<-ctx.Done()
		return domain.ProbeRuntimeLease{}, ctx.Err()
	}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{})
	svc.renewInterval = time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	err := svc.withRuntime(ctx, "33333333-3333-4333-8333-333333333333", func(ctx context.Context, _ domain.ProbeRuntimeLease) error {
		select {
		case <-started:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil || ctx.Err() != nil || leases.runtimeReleased.Load() != 1 {
		t.Fatalf("successful action overwritten by cleanup cancellation: %v", err)
	}
}

func TestProbeConnectorEnrollmentWhileWorkerOwnsRuntime(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{runtimeAcquireErr: ports.ErrConflict}
	called := false
	svc := newConnectorTestService(t, connections, leases, connectorTransport{enroll: func(context.Context, domain.ProbeCredentialMetadata, string, string) error { called = true; return nil }})
	prepareConnectorTest(t, svc)
	if err := svc.Enroll(t.Context(), connections.value.ProbeID, "one-time-token"); err != nil || !called {
		t.Fatalf("background runtime starved operator enrollment: %v called=%v", err, called)
	}
}

func TestProbeConnectorEnrollmentRechecksPreparedAuthority(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{enroll: func(context.Context, domain.ProbeCredentialMetadata, string, string) error {
		t.Error("disabled preparation reached network")
		return nil
	}})
	prepareConnectorTest(t, svc)
	connections.prepareErr = ports.ErrConflict
	if err := svc.Enroll(t.Context(), connections.value.ProbeID, "one-time-token"); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("enrollment ignored changed registration: %v", err)
	}
}
