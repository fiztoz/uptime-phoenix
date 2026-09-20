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
	connected              atomic.Bool
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
}

func (f connectorTransport) Run(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error, applied func(context.Context, domain.ProbeActiveConfig) error) error {
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
	svc, err := NewProbeConnectorService(connections, leases, connectorProtector{}, configs, transport,
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
	if connections.value.State != "prepared" || leases.released.Load() != 7 {
		t.Fatal("failed enrollment destroyed recovery or retained lease")
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
	if _, err := svc.connectOnce(t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrNotFound) {
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
	if _, err := svc.connectOnce(t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrConflict) {
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
	_, err := svc.connectOnce(ctx, connections.value.ProbeID)
	if err == nil || ctx.Err() != nil || !canceled.Load() || time.Since(start) < 14*time.Second || leases.released.Load() != 7 || connections.activated != 1 {
		t.Fatal("lease renewal did not revoke the live socket and join its owner")
	}
}
