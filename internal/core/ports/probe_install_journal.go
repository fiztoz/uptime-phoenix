package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeInstallOperationRepository persists initial-install intent, never installed success.
// Every method validates the complete stored graph before expiry reconciliation.
// Mutations lock the operation, then sample DB UTC time and fence stored owner/state/
// deadline atomically. No renewal/takeover exists. Missing IDs return ErrNotFound;
// identity/state/owner/outcome conflicts return ErrConflict; corruption is ErrValidation.
// Returned receipts own their slices; no external I/O occurs within a transaction.
type ProbeInstallOperationRepository interface {
	CreateInstall(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (operation *domain.ProbeInstallOperation, created bool, err error)
	ObserveInstall(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
	BeginInstall(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error)
	RequestInstallCancellation(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error)
	// Finish requires a joined owner. Canceled also requires stored cancel_requested.
	// Failed/canceled reject in-flight/unknown actions.
	// Interrupted converts in-flight to unknown. Exact terminal retries never mutate history.
	FinishInstall(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error)
}

// ProbeInstallResourceJournal records one attempt per canonical resource.
// It is DB intent/report authority only, not fencing of remote SSH processes or
// proof of actual remote resource ownership; a future executor must verify those.
type ProbeInstallResourceJournal interface {
	// Reserve returns created=true only for a newly committed in-flight intent whose
	// predecessors are already committed applied. Exact retries return created=false,
	// including historical/expired outcomes, and NEVER authorize another invocation.
	ReserveInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (operation *domain.ProbeInstallOperation, created bool, err error)
	// Record accepts an exact stored attempt only after the invocation joins.
	// Applied/not_started/unknown reports are immutable; exact outcome retries read history.
	// Late results never replace expiry uncertainty. Last applied changes running to
	// resources_ready (not terminal/success); cancel intent stays cancel_requested.
	RecordInstallResource(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error)
}
