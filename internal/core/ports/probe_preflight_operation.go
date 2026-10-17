package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeHostInspectorFactory parses an ephemeral key and explicit host pin for
// one operation. The returned fingerprint is of the PUBLIC authentication key.
// It must not retain the caller's private-key buffer or persist any credential.
type ProbeHostInspectorFactory interface {
	NewInspector(ctx context.Context, privateKey []byte, hostPin string) (ProbeHostInspector, string, error)
}

// ProbePreflightRepository records discovery only, independently of registered
// probe operations. Every mutation is transactional, uses the DB UTC clock, and
// checks current stored authority; no lease/owner takeover or renewal exists.
// Missing IDs return ErrNotFound; immutable identity/outcome/authority conflicts
// return ErrConflict. Invalid/corrupt rows fail closed with domain.ErrValidation.
type ProbePreflightRepository interface {
	// Create commits queued work before acknowledgement. An exact immutable
	// identity retry returns its original row (including original owner/deadline)
	// and created=false; a changed selector, actor or public key conflicts.
	CreatePreflight(ctx context.Context, request domain.ProbePreflightIdentity, ownerID string) (operation *domain.ProbePreflightOperation, created bool, err error)
	// Observe atomically marks any nonterminal hard-expired row interrupted
	// before returning it. Reads never create work or infer successful completion.
	ObservePreflight(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error)
	// Begin requires the exact stored owner, queued state and unexpired deadline.
	BeginPreflight(ctx context.Context, operationID, ownerID string) (*domain.ProbePreflightOperation, error)
	// Finish checks owner/state/expiry in the same transaction. Passed/failed
	// require running, canceled requires cancel_requested AFTER executor join;
	// interrupted may be recorded by the joined owner on shutdown. Exact terminal
	// retries are idempotent, but cannot replace any historical terminal outcome.
	FinishPreflight(ctx context.Context, operationID, ownerID string, outcome domain.ProbePreflightOutcome) (*domain.ProbePreflightOperation, error)
	// RequestCancellation persists intent, not a claim that SSH work stopped.
	// Terminal rows remain unchanged. Cancellation on another API replica is
	// observed by the owner; disappearance/expiry means interrupted, not canceled.
	RequestPreflightCancellation(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error)
}
