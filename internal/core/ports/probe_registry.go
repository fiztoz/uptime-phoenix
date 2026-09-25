package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeRegistryRepository stores registration metadata without credentials.
// Create accepts remote registrations only and allocates a canonical UUID when
// the caller omits the identity. Update preserves identity, requires the
// expected revision, and rejects mutation of the reserved local row.
// ErrConflict identifies duplicate identity/key or stale revision. Invalid input
// wraps domain.ErrValidation; missing rows return ErrNotFound.
type ProbeRegistryRepository interface {
	Create(ctx context.Context, probe *domain.Probe) error
	GetByID(ctx context.Context, id string) (*domain.Probe, error)
	// GetByKey returns the registration carrying the given stable probe key,
	// including the reserved local row. The key is the portable identity used
	// by declarative documents; the UUID is runtime identity.
	GetByKey(ctx context.Context, key string) (*domain.Probe, error)
	List(ctx context.Context) ([]domain.Probe, error)
	Update(ctx context.Context, probe *domain.Probe, expectedRevision int64) error
}

// MonitorProbeAssignmentRepository stores complete desired sets transactionally.
// InitializeLocal creates revision/generation one for an existing uninitialized
// monitor; repeated initialization returns the current set without changing it.
// SQLite/MariaDB monitor Create inserts the local assignment in the same
// transaction, so ordinary create/clone/import/restore paths initialize it.
// Replace requires a positive expected revision, at least one distinct enabled
// registered probe, and a valid policy. It validates all members before changing
// any. Retained generations are stable, removed members retain tombstones, and
// re-adding a member increments its generation. No-op replacement preserves the
// revision. ErrConflict reports stale/exhausted revisions or generations.
// GetByMonitorID never silently creates assignments or reroutes execution.
// ExecutableByLocal reports which of the given monitors the hub worker may run
// and their active local assignment generations: missing assignment sets are
// legacy-local (generation 1); sets without an active local probe are remote-only
// and must not be claimed or checked by the hub scheduler.
// ListHistory returns persisted membership/policy intervals overlapping [from,to),
// ordered by effective start then row ID. Boundaries are UTC and are not clipped.
// An empty history means unknown membership, never today's set applied backwards.
// Successful initialization/replacement commits history with the desired set.
//
// Restore commits a complete desired set that arrived from a declarative
// document (backup import or config apply). It is Replace with one documented
// exception: members must be registered but need not be enabled, because a
// restored or just-declared identity is disabled or unenrolled until an operator
// registers and enrolls it. Callers that accept live operator input must keep
// using Replace so a disabled registration cannot be handed new work. Restore
// never invents membership: every member must exist, and a set without `local`
// leaves the monitor remote-only exactly like Replace.
type MonitorProbeAssignmentRepository interface {
	InitializeLocal(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error)
	GetByMonitorID(ctx context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error)
	Replace(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy) (*domain.MonitorProbeAssignments, error)
	Restore(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error)
	ExecutableByLocal(ctx context.Context, monitorIDs []int64) (map[int64]int64, error)
	ListHistory(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.AssignmentInterval, error)
}
