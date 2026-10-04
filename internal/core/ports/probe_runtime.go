package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeRuntimeLeaseRepository keeps one owner across connector attempts. Leases
// use the DB clock for ownership only, never for watchdog elapsed health timing.
// Child connector leases cannot outlive this lease; release/takeover fences them.
type ProbeRuntimeLeaseRepository interface {
	AcquireRuntime(ctx context.Context, probeID, ownerID string) (domain.ProbeRuntimeLease, error)
	RenewRuntime(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeRuntimeLease, error)
	ReleaseRuntime(ctx context.Context, lease domain.ProbeRuntimeLease) error
	AcquireRuntimeConnector(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeConnectorLease, error)
}
