package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeReplayAuthorizer evaluates business and historical authorization rules
// against captured authority facts, returning a stable rejection code if unauthorized.
type ProbeReplayAuthorizer interface {
	AuthorizeEvent(ctx context.Context, facts domain.ProbeReplayAuthorityFacts, event domain.ProbeReplayEvent, now time.Time) (rejectionCode string, ok bool)
}

// ProbeReplayRepository executes the transactional ingest of a replay batch.
type ProbeReplayRepository interface {
	IngestReplayBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch, authorizer ProbeReplayAuthorizer) (*domain.ProbeReplayResult, error)
	GetCursor(ctx context.Context, probeID, streamID string) (int64, error)
}

// ProbeReplayService coordinates batch ingestion with the repository and authorizer.
type ProbeReplayService interface {
	ProcessBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)
}
