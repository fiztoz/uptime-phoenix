package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RegionalBrowserRepository reads persisted current samples without scanning a
// history window or synthesizing latency from a current snapshot.
type RegionalBrowserRepository interface {
	LatestObservations(ctx context.Context, monitorID int64) ([]domain.RegionalObservation, error)
}
