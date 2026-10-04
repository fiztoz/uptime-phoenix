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
