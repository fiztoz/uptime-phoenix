package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RegionalCommitRepository atomically records one local/edge evaluation.
// Implementations must persist observation and regional state together.
// Incident is optional. The method performs no provider I/O and does not
// advance a remote ingest cursor.
type RegionalCommitRepository interface {
	Commit(ctx context.Context, commit domain.RegionalCommit) error
	GetState(ctx context.Context, monitorID int64, probeID string) (*domain.RegionalState, error)
	// ListStates returns current evidence for one monitor, ordered by probe_id.
	// Missing monitors yield an empty slice, not rows from other monitors.
	ListStates(ctx context.Context, monitorID int64) ([]domain.RegionalState, error)
	// ListObservations returns ordered regional history in [from, to].
	// Implementations must force from/to to UTC at the database boundary.
	ListObservations(ctx context.Context, monitorID int64, probeID string, from, to time.Time) ([]domain.RegionalObservation, error)
}

// ProbeIngestRepository atomically accepts a contiguous remote telemetry prefix.
// Duplicate sequences at or below the committed cursor are no-ops. Undeclared
// gaps and identity mismatches return ErrConflict. GetCursor never invents a stream.
type ProbeIngestRepository interface {
	Ingest(ctx context.Context, batch domain.ProbeIngestBatch) (committedSeq int64, err error)
	GetCursor(ctx context.Context, probeID, streamID string) (committedSeq int64, err error)
}

// ProbeCommandRepository stores command identity and results without secrets.
type ProbeCommandRepository interface {
	Put(ctx context.Context, command *domain.ProbeCommand) error
	Get(ctx context.Context, commandID string) (*domain.ProbeCommand, error)
}
