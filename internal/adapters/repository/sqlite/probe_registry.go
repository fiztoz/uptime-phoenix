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

// RegionalCommitRepo is the SQLite regional observation/state store.
type RegionalCommitRepo struct {
	*repository.RegionalCommitStore
}

// NewRegionalCommitRepo creates a SQLite regional commit repository.
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

// ProbeConfigRepo retains protected prepared configuration on sqlite.
type ProbeConfigRepo struct{ *repository.ProbeConfigStore }

// NewProbeConfigRepo creates a prepared snapshot repository, without activation.
func NewProbeConfigRepo(db *bun.DB) *ProbeConfigRepo {
	return &ProbeConfigRepo{repository.NewProbeConfigStore(db)}
}

var _ ports.ProbeConfigRepository = (*ProbeConfigRepo)(nil)

// ProbeInstallationRepo manages singleton installation identity and key confirmation on sqlite.
type ProbeInstallationRepo struct {
	*repository.ProbeInstallationStore
}

// NewProbeInstallationRepo creates an installation repository.
func NewProbeInstallationRepo(db *bun.DB) *ProbeInstallationRepo {
	return &ProbeInstallationRepo{repository.NewProbeInstallationStore(db)}
}

var _ ports.ProbeInstallationRepository = (*ProbeInstallationRepo)(nil)

// ProbeActivationRepo manages active configuration and applied receipts on sqlite.
type ProbeActivationRepo struct {
	*repository.ProbeActivationStore
}

// NewProbeActivationRepo creates an activation repository.
func NewProbeActivationRepo(db *bun.DB, encoder ports.LocalProbeConfigEncoder) *ProbeActivationRepo {
	return &ProbeActivationRepo{repository.NewProbeActivationStore(db, encoder)}
}

var _ ports.ProbeConfigActivationRepository = (*ProbeActivationRepo)(nil)
