package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeHistoryProjector computes a window without database or provider I/O.
// It may be called again after a serialization retry.
type ProbeHistoryProjector interface {
	HistoryFreshness(domain.Monitor) (time.Duration, error)
	ProjectHistory(context.Context, domain.ProbeHistoryWork) (domain.ProbeHistoryProjection, error)
}

// ProbeHistoryWorkRepository owns bounded gap expansion and closed-window work.
// Evidence read, result writes and work consumption must share a transaction
// that serializes against source re-marking. Provider I/O is never permitted.
type ProbeHistoryWorkRepository interface {
	ProcessHistoryWork(context.Context, time.Time, int, ProbeHistoryProjector) (int, error)
}

// ProbeHistoryReadRepository supplies a coherent bounded historical window,
// including sequence-leading seeds that predate ordinary freshness lookback.
type ProbeHistoryReadRepository interface {
	ReadHistoryEvidence(context.Context, int64, time.Time, time.Time, ProbeHistoryProjector) (domain.OverallHistoryInput, error)
}
