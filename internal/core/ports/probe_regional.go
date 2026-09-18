package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RegionalCommitRepository atomically records one local/edge evaluation.
// Implementations must persist observation and regional state together.
// Incident is optional; delivery intents require its matching transition and
// are committed in the same transaction. The method performs no provider I/O and does not
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
	// ListObservationsInRange returns every probe's ordered history in [from, to].
	ListObservationsInRange(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.RegionalObservation, error)
}

// MonitorHealthProjectionRepository stores overall snapshots, history, and dirty work.
// Regional reads stay on RegionalCommitRepository; overall reads use these rows or
// ReconstructOverallHistory. Implementations force UTC at the database boundary.
type MonitorHealthProjectionRepository interface {
	PutHealthState(ctx context.Context, state *domain.MonitorHealthState) error
	GetHealthState(ctx context.Context, monitorID int64) (*domain.MonitorHealthState, error)
	ReplaceHealthHistory(ctx context.Context, monitorID int64, from, to time.Time, intervals []domain.MonitorHealthInterval) error
	ListHealthHistory(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.MonitorHealthInterval, error)
	MarkDirty(ctx context.Context, buckets []domain.DirtyBucket) error
	ListDirty(ctx context.Context, resolution string, limit int) ([]domain.DirtyBucket, error)
	ClearDirty(ctx context.Context, buckets []domain.DirtyBucket) error
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

// ProbeIncidentRepository stores source-owned regional incidents. Hub mirror IDs
// are assigned on insert and are distinct from source_alert_id. Put is idempotent
// at the same transition version and rejects lower versions or identity changes.
// Aggregate/group incidents are not accepted. The method performs no provider I/O.
type ProbeIncidentRepository interface {
	PutIncident(ctx context.Context, incident *domain.RegionalIncident) error
	GetIncident(ctx context.Context, sourceAlertID string) (*domain.RegionalIncident, error)
	ListIncidentsByMonitor(ctx context.Context, monitorID int64) ([]domain.RegionalIncident, error)
}

// ProbeDeliveryRepository stores source delivery outcomes. PutDelivery correlates
// an already persisted incident transition and may advance attempt without changing
// delivery identity. It never sends a notification.
type ProbeDeliveryRepository interface {
	PutDelivery(ctx context.Context, delivery *domain.RegionalDelivery) error
	GetDelivery(ctx context.Context, deliveryID string) (*domain.RegionalDelivery, error)
	ListDeliveriesByIncident(ctx context.Context, sourceAlertID string) ([]domain.RegionalDelivery, error)
}
