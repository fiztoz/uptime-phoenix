package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type connectorContacts struct {
	mu     sync.Mutex
	leases []domain.ProbeConnectorLease
	err    error
}

func (f *connectorContacts) RecordConnectorContact(_ context.Context, lease domain.ProbeConnectorLease) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leases = append(f.leases, lease)
	return f.err
}

func (f *connectorContacts) recorded() []domain.ProbeConnectorLease {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.ProbeConnectorLease(nil), f.leases...)
}

// TestProbeConnectorRecordsContactOnlyForAcceptedHealth pins #133: last contact
// is persisted once per authenticated health frame the session accepted, for
// that session's exact lease. Lease renewal and configuration refresh are hub
// maintenance and never record contact, and a refused fenced callback records
// nothing. A failing record is diagnostic only and never ends the session.
func TestProbeConnectorRecordsContactOnlyForAcceptedHealth(t *testing.T) {
	t.Run("PerAcceptedHealthFrame", func(t *testing.T) {
		connections, leases, contacts := &connectorConnections{}, &connectorLeases{}, &connectorContacts{}
		renewalsObserved := make(chan struct{})
		svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, in domain.ProbeSessionInput, ready func(context.Context) error) error {
			for range 3 {
				if err := ready(ctx); err != nil {
					return err
				}
			}
			// Several renewal ticks pass with no inbound frame.
			select {
			case <-renewalsObserved:
			case <-ctx.Done():
				return ctx.Err()
			}
			return errors.New("peer closed")
		}})
		svc.renewInterval = 5 * time.Millisecond
		svc.SetContactRecorder(contacts)
		prepareConnectorTest(t, svc)
		go func() {
			time.Sleep(60 * time.Millisecond)
			close(renewalsObserved)
		}()
		if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); err == nil || err.Error() != "peer closed" {
			t.Fatalf("session result: %v", err)
		}
		got := contacts.recorded()
		if len(got) != 3 {
			t.Fatalf("contact records = %d, want one per accepted health frame (3)", len(got))
		}
		for _, lease := range got {
			if lease.Generation != 7 || lease.OwnerID != "22222222-2222-4222-8222-222222222222" || lease.ProbeID != connections.value.ProbeID {
				t.Fatalf("contact recorded for the wrong session: %+v", lease)
			}
		}
	})
	t.Run("RefusedCallbackRecordsNothing", func(t *testing.T) {
		connections, leases, contacts := &connectorConnections{}, &connectorLeases{connectedErr: ports.ErrConflict}, &connectorContacts{}
		svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, _ domain.ProbeSessionInput, ready func(context.Context) error) error {
			return ready(ctx)
		}})
		svc.SetContactRecorder(contacts)
		prepareConnectorTest(t, svc)
		if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("stale callback accepted: %v", err)
		}
		if n := len(contacts.recorded()); n != 0 {
			t.Fatalf("stale session recorded contact %d times", n)
		}
	})
	t.Run("RecordFailureIsNotFatal", func(t *testing.T) {
		connections, leases, contacts := &connectorConnections{}, &connectorLeases{}, &connectorContacts{err: errors.New("database unavailable")}
		svc := newConnectorTestService(t, connections, leases, connectorTransport{run: func(ctx context.Context, _ domain.ProbeSessionInput, ready func(context.Context) error) error {
			if err := ready(ctx); err != nil {
				return err
			}
			return ready(ctx)
		}})
		svc.SetContactRecorder(contacts)
		prepareConnectorTest(t, svc)
		if err := connectorTestOnce(svc, t.Context(), connections.value.ProbeID); err != nil {
			t.Fatalf("diagnostic record failure ended the session: %v", err)
		}
		if n := len(contacts.recorded()); n != 2 || !leases.connected.Load() {
			t.Fatalf("records = %d connected = %v", n, leases.connected.Load())
		}
	})
}
