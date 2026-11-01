package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeConnectorLeaseRepository uses the hub database clock for 60-second leases.
// Workers renew every 15 seconds. All mutations compare owner and generation;
// stale close callbacks cannot disconnect a newer session. Acquisition never
// authorizes checks or provider delivery on behalf of the remote probe.
type ProbeConnectorLeaseRepository interface {
	AcquireConnector(ctx context.Context, probeID, ownerID string) (domain.ProbeConnectorLease, error)
	RenewConnector(ctx context.Context, lease domain.ProbeConnectorLease) (domain.ProbeConnectorLease, error)
	ReleaseConnector(ctx context.Context, lease domain.ProbeConnectorLease) error
	SetConnectorConnected(ctx context.Context, lease domain.ProbeConnectorLease, connected bool) error
}

// ProbeConnectorContactRecorder persists hub-observed probe contact for one
// fenced connector session. Implementations stamp the hub database clock, refuse
// stale or expired sessions with ErrConflict and never move the value backward.
// Hub-side maintenance (lease renewal, configuration refresh, the watchdog) must
// never call it: only an authenticated frame the owning hub accepted is contact.
type ProbeConnectorContactRecorder interface {
	RecordConnectorContact(ctx context.Context, lease domain.ProbeConnectorLease) error
}
