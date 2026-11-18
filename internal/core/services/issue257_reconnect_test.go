package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestIssue257ReconnectLifecycle exercises the owned connector loop, including
// failed dials, a health-confirmed short session, and cancellation during wait.
func TestIssue257ReconnectLifecycle(t *testing.T) {
	connections, leases := &connectorConnections{}, &connectorLeases{}
	attempt := 0
	transport := connectorTransport{run: func(ctx context.Context, _ domain.ProbeSessionInput, ready func(context.Context) error) error {
		attempt++
		if attempt == 2 || attempt == 4 {
			if err := ready(ctx); err != nil {
				return err
			}
			if attempt == 2 {
				select {
				case <-time.After(1200 * time.Millisecond):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return errors.New("connection dropped")
	}}
	svc := newConnectorTestService(t, connections, leases, transport)
	prepareConnectorTest(t, svc)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var failures []int
	svc.delay = func(f int, _ time.Duration) time.Duration {
		failures = append(failures, f)
		if len(failures) == 4 {
			cancel()
		}
		return time.Second
	}
	svc.ownedConnectLoop(ctx, domain.ProbeRuntimeLease{ProbeID: connections.value.ProbeID, OwnerID: svc.ownerID, Epoch: 1}, nil, nil)
	if len(failures) != 4 || failures[0] != 1 || failures[1] != 1 || failures[2] != 2 || failures[3] != 3 {
		t.Fatalf("failure sequence = %v, want [1 1 2 3]", failures)
	}
	if leases.released.Load() != 7 {
		t.Fatal("connector lease not released")
	}
}
