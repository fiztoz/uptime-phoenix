package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeStateRepository captures current evidence under one durable session fence.
type EdgeStateRepository interface {
	ReadCurrentSnapshot(ctx context.Context, fence domain.EdgeReplayFence, at time.Time) (domain.EdgeCurrentSnapshot, error)
}

// ProbeStateRepository atomically reconciles complete current evidence and its
// durable receipt. It cannot acknowledge history or create provider work.
type ProbeStateRepository interface {
	ApplyCurrentSnapshot(ctx context.Context, session domain.ProbeReplaySession, snapshot domain.ProbeCurrentSnapshot, authorizer ProbeStateAuthorizer) (*domain.ProbeStateReceipt, error)
}

// ProbeStateAuthorizer is the central access policy for exact config membership.
type ProbeStateAuthorizer interface {
	AuthorizeCurrentSnapshot(ctx context.Context, facts domain.ProbeStateAuthorityFacts, snapshot domain.ProbeCurrentSnapshot) bool
}

// ProbeStateService validates a current-state request before persistence.
type ProbeStateService interface {
	ApplySnapshot(ctx context.Context, session domain.ProbeReplaySession, snapshot domain.ProbeCurrentSnapshot) (*domain.ProbeStateReceipt, error)
}
