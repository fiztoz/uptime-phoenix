package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeRegistryRepository stores registration metadata without credentials.
// Create accepts remote registrations only. Update preserves identity, requires
// the expected revision, and rejects mutation of the reserved local row.
// ErrConflict identifies duplicate identity/key or stale revision. Invalid input
// wraps domain.ErrValidation; missing rows return ErrNotFound.
type ProbeRegistryRepository interface {
	Create(ctx context.Context, probe *domain.Probe) error
	GetByID(ctx context.Context, id string) (*domain.Probe, error)
	List(ctx context.Context) ([]domain.Probe, error)
	Update(ctx context.Context, probe *domain.Probe, expectedRevision int64) error
}

// MonitorProbeAssignmentRepository stores complete desired sets transactionally.
// InitializeLocal creates revision/generation one for an existing uninitialized
// monitor; repeated initialization returns the current set without changing it.
// New monitor creation has not yet been wired to this foundation.
// Replace requires a positive expected revision, at least one distinct enabled
// registered probe, and a valid policy. It validates all members before changing
// any. Retained generations are stable, removed members retain tombstones, and
// re-adding a member increments its generation. No-op replacement preserves the
// revision. ErrConflict reports stale/exhausted revisions or generations.
// GetByMonitorID never silently creates assignments or reroutes execution.
type MonitorProbeAssignmentRepository interface {
	InitializeLocal(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error)
	GetByMonitorID(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error)
	Replace(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy) (*domain.MonitorProbeAssignments, error)
}
