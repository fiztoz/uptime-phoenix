package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeInstallationRepository manages the singleton installation identity and key confirmation record.
type ProbeInstallationRepository interface {
	// Get returns the singleton installation record, or ErrNotFound if not yet initialized.
	Get(ctx context.Context) (*domain.ProbeInstallation, error)

	// Initialize atomically inserts the singleton installation record.
	// If an identical record already exists, it returns the existing record idempotently.
	// If a different record exists, it returns ErrConflict.
	Initialize(ctx context.Context, inst domain.ProbeInstallation) (*domain.ProbeInstallation, error)

	// HasSnapshots returns true if any prepared probe configuration snapshots exist.
	HasSnapshots(ctx context.Context) (bool, error)

	// VerifyRetainedSnapshots iterates through all retained probe configuration snapshots in bounded batches,
	// verifying that each snapshot's hub_id matches the trusted hubID and invoking check on its metadata and ciphertext.
	VerifyRetainedSnapshots(ctx context.Context, hubID string, check func(metadata domain.ProbeConfigMetadata, payload []byte) error) error
}
