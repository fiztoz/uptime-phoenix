package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeLifecycleRepository commits revocation receipts with registration and
// lease fencing atomically. Soft deletion rejects active assignments under the
// same probe lock used by assignment replacement and retains attribution.
type ProbeLifecycleRepository interface {
	RevokeProbe(ctx context.Context, probeID, operationID string, at time.Time) (*domain.ProbeOperation, error)
	GetRevocation(ctx context.Context, operationID string) (*domain.ProbeOperation, error)
	DeleteProbe(ctx context.Context, probeID, operationID string, at time.Time) error
}
