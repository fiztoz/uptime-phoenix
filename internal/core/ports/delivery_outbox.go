package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// DeliveryOutboxRepository manages source-owned availability work. Enqueue is
// available only through an atomic regional/local commit. Mirroring telemetry
// never enqueues work. Probe scope is a storage boundary, not authentication.
type DeliveryOutboxRepository interface {
	// ClaimDeliveries leases at most limit due intents in deterministic order.
	// Expired leases are reclaimable with a new token and incremented attempt.
	// No provider I/O occurs here; consumers must revalidate before sending.
	ClaimDeliveries(ctx context.Context, probeID string, at time.Time, lease time.Duration, limit int) ([]domain.QueuedDelivery, error)
	// FinishDelivery atomically updates the queue and corresponding outcome.
	// Only the current, unexpired attempt may complete. An identical retry of a
	// completed receipt is a no-op; conflicting or superseded receipts fail.
	FinishDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error
	// GetDeliveryIntent reads only the requested probe's source work.
	GetDeliveryIntent(ctx context.Context, probeID, deliveryID string) (*domain.QueuedDelivery, error)
}

// EscalationOutboxRepository commits a due step without provider I/O. A stale
// claim cannot enqueue work or advance progress. Retrying after commit is a no-op.
type EscalationOutboxRepository interface {
	CommitEscalationStep(ctx context.Context, step domain.EscalationStepCommit) (bool, error)
}
