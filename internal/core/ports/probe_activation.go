package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// LocalActivationParams carries verified candidate metadata and expected active state.
type LocalActivationParams struct {
	Target                 domain.ProbeConfigTarget
	Revision               int64
	SHA256                 string
	ExpectedActiveRevision int64
	AppliedAt              time.Time
	AssignmentCount        int
}

// ProbeConfigActivationRepository manages active configuration pointers and applied receipts.
// ActivateLocal executes under an atomic transaction with source freshness recheck.
type ProbeConfigActivationRepository interface {
	// GetActive returns the current active configuration pointer for probeID.
	// Returns ErrNotFound if no configuration has been activated.
	GetActive(ctx context.Context, probeID string) (*domain.ProbeActiveConfig, error)

	// GetReceipt returns the durable receipt for probeID and revision.
	// Returns ErrNotFound if the revision was never applied.
	GetReceipt(ctx context.Context, probeID string, revision int64) (*domain.ProbeActiveConfig, error)

	// ActivateLocal atomically verifies source freshness, checks expectedActiveRevision,
	// verifies trusted installation authority, checks candidate snapshot hash,
	// and commits the active pointer and receipt together.
	// Returns the applied receipt, or ErrConflict if source changed, revision is stale,
	// or expectedActiveRevision does not match.
	ActivateLocal(ctx context.Context, params LocalActivationParams) (*domain.ProbeActiveConfig, error)
}
