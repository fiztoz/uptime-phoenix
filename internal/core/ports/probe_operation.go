package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeOperationRepository durably records administrative operations. Create
// must commit before any caller acknowledges work (202 without a persisted row
// is forbidden), and Finish records the terminal hub-side outcome with a
// bounded redacted error. Get reads one exact receipt; reads never create.
//
// The repository validates the complete receipt invariant and wraps invalid
// input in domain.ErrValidation; a missing operation returns ErrNotFound.
type ProbeOperationRepository interface {
	CreateOperation(ctx context.Context, operation domain.ProbeOperation) error
	GetOperation(ctx context.Context, operationID string) (*domain.ProbeOperation, error)
	FinishOperation(ctx context.Context, operationID string, status, phase string, opErr *domain.ProbeOperationError, at time.Time) error
}
