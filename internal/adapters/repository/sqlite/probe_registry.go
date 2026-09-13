package sqlite

import (
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeRegistryRepo is the SQLite registration foundation, without runtime wiring.
type ProbeRegistryRepo struct{ *repository.ProbeRegistryStore }

// NewProbeRegistryRepo creates a SQLite probe registry.
func NewProbeRegistryRepo(db *bun.DB) *ProbeRegistryRepo {
	return &ProbeRegistryRepo{repository.NewProbeRegistryStore(db)}
}

// ProbeAssignmentRepo is the SQLite atomic desired-assignment store.
type ProbeAssignmentRepo struct {
	*repository.ProbeAssignmentStore
}

// NewProbeAssignmentRepo creates a SQLite probe assignment repository.
func NewProbeAssignmentRepo(db *bun.DB) *ProbeAssignmentRepo {
	return &ProbeAssignmentRepo{repository.NewProbeAssignmentStore(db)}
}

var (
	_ ports.ProbeRegistryRepository          = (*ProbeRegistryRepo)(nil)
	_ ports.MonitorProbeAssignmentRepository = (*ProbeAssignmentRepo)(nil)
)
