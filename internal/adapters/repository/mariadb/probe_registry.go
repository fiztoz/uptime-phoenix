package mariadb

import (
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeRegistryRepo is the MariaDB registration foundation, without runtime wiring.
type ProbeRegistryRepo struct{ *repository.ProbeRegistryStore }

// NewProbeRegistryRepo creates a MariaDB probe registry.
func NewProbeRegistryRepo(db *bun.DB) *ProbeRegistryRepo {
	return &ProbeRegistryRepo{repository.NewProbeRegistryStore(db)}
}

// ProbeAssignmentRepo is the MariaDB atomic desired-assignment store.
type ProbeAssignmentRepo struct {
	*repository.ProbeAssignmentStore
}

// NewProbeAssignmentRepo creates a MariaDB probe assignment repository.
func NewProbeAssignmentRepo(db *bun.DB) *ProbeAssignmentRepo {
	return &ProbeAssignmentRepo{repository.NewProbeAssignmentStore(db)}
}

// RegionalCommitRepo is the MariaDB regional observation/state store.
type RegionalCommitRepo struct {
	*repository.RegionalCommitStore
}

// NewRegionalCommitRepo creates a MariaDB regional commit repository.
func NewRegionalCommitRepo(db *bun.DB) *RegionalCommitRepo {
	return &RegionalCommitRepo{repository.NewRegionalCommitStore(db)}
}

var (
	_ ports.ProbeRegistryRepository           = (*ProbeRegistryRepo)(nil)
	_ ports.MonitorProbeAssignmentRepository  = (*ProbeAssignmentRepo)(nil)
	_ ports.RegionalCommitRepository          = (*RegionalCommitRepo)(nil)
	_ ports.ProbeIngestRepository             = (*RegionalCommitRepo)(nil)
	_ ports.ProbeIncidentRepository           = (*RegionalCommitRepo)(nil)
	_ ports.ProbeDeliveryRepository           = (*RegionalCommitRepo)(nil)
	_ ports.DeliveryOutboxRepository          = (*RegionalCommitRepo)(nil)
	_ ports.MonitorHealthProjectionRepository = (*RegionalCommitRepo)(nil)
)
