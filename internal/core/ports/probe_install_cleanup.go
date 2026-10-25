package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeCleanupRepository records cleanups of install operations with the
// install journal's fencing: every transition is serialized on the install
// operation row (the shared authority lock), samples the DB clock after that
// lock, and requires the stored owner, state and an unexpired deadline. All
// receipts are complete, valid domain.ProbeCleanup values.
type ProbeCleanupRepository interface {
	// CreateCleanup requires the install operation to exist with exactly
	// identity.Install, in a cleanup-eligible state
	// (domain.ProbeInstallCleanupEligible), with no activation or a terminal
	// one, no nonterminal cleanup and no cleaned cleanup; otherwise
	// domain.ErrProbeCleanupIneligible (ErrNotFound for a missing install).
	// created=true only for a new queued row under ownerID; an exact retry of
	// the same cleanup ID returns the stored receipt (created=false); the same
	// cleanup ID with a changed identity is ErrConflict.
	CreateCleanup(ctx context.Context, identity domain.ProbeCleanupIdentity, ownerID string) (*domain.ProbeCleanup, bool, error)
	// ObserveCleanup reconciles hard expiry (queued or running past the
	// deadline becomes interrupted/deadline_expired).
	ObserveCleanup(ctx context.Context, cleanupID string) (*domain.ProbeCleanup, error)
	// LatestCleanup returns the newest cleanup of an install operation
	// (created_at, then cleanup_id, descending), reconciled like Observe, or
	// ErrNotFound when it has none.
	LatestCleanup(ctx context.Context, operationID string) (*domain.ProbeCleanup, error)
	// BeginCleanup is the single queued-to-running transition for ownerID; it
	// reserves attemptID, which must be globally unique.
	BeginCleanup(ctx context.Context, cleanupID, ownerID, attemptID string) (*domain.ProbeCleanup, error)
	// RecordCleanup settles a running cleanup for its exact owner and
	// attempt: applied becomes cleaned, not_started failed/not_started and
	// unknown interrupted/outcome_unknown. Past the deadline it reconciles to
	// interrupted/deadline_expired instead and returns that receipt.
	RecordCleanup(ctx context.Context, cleanupID, ownerID, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeCleanup, error)
	// AbandonCleanup ends a still-queued cleanup of ownerID as
	// interrupted/owner_interrupted; it never touches a running cleanup.
	AbandonCleanup(ctx context.Context, cleanupID, ownerID string) (*domain.ProbeCleanup, error)
}

// ProbeCleanupAdmissionRepository irreversibly admits one cleanup attempt:
// the cleanup must be running under the intent's owner with the intent's
// attempt, its install identity must equal the intent's, and the deadline
// must not have passed. Every replay (same attempt or same nonce) is refused,
// exactly like AdmitHostAction.
type ProbeCleanupAdmissionRepository interface {
	AdmitCleanupAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error)
}

// ProbeCleanupHostSession runs the authorized cleanup host action over one
// transient SSH key. Outcomes follow the install executor: applied,
// not_started (nothing reached the helper) or unknown.
type ProbeCleanupHostSession interface {
	Cleanup(ctx context.Context, intent domain.ProbeHostActionIntent) domain.ProbeInstallResourceOutcome
}

// ProbeCleanupSessionFactory parses one transient deployment key and returns
// a cleanup host session plus the key's PUBLIC fingerprint. It performs no
// SSH I/O and creates nothing durable.
type ProbeCleanupSessionFactory interface {
	NewCleanupSession(ctx context.Context, privateKey []byte, hostFingerprint string) (ProbeCleanupHostSession, string, error)
}
