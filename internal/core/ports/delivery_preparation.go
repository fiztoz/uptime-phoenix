package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeDeliveryPreparationCodec maps the closed wire request to bounded core data.
type ProbeDeliveryPreparationCodec interface {
	EncodeDeliveryPreparation(context.Context, domain.ProbeDeliveryPreparation) ([]byte, error)
	DecodeDeliveryPreparation(context.Context, []byte) (domain.ProbeDeliveryPreparation, error)
}

// RemoteDeliveryPreparationIssuer records requests from committed regional facts,
// current desired binding and database session authority, never queue ordering.
type RemoteDeliveryPreparationIssuer interface {
	IssueRemoteDeliveryPreparations(context.Context, domain.ProbeReplaySession, []domain.ProbeDeliveryModeApplication) error
}

// EdgeDeliveryPreparationRepository persists preparation and command receipt
// atomically. A nil capture durably rejects unavailable/stale configuration.
type EdgeDeliveryPreparationRepository interface {
	ReadDeliveryPreparationReceipt(context.Context, domain.EdgeCommandAuthority, domain.ProbeDeliveryPreparation) (*domain.ProbeCommandOutcome, error)
	PrepareDelivery(context.Context, domain.EdgeCommandAuthority, domain.ProbeDeliveryPreparation, *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error)
}

// LocalDeliveryPreparationReader reads bounded request records for an exact
// selected protected local configuration. Absence is unreported, never application.
// Implementations independently validate committed source/document prerequisites.
type LocalDeliveryPreparationReader interface {
	ReadLocalDeliveryPreparations(context.Context, domain.ProbeActiveConfig) ([]domain.LocalDeliveryPreparation, error)
}

// EdgeDeliveryPreparationExecutor authenticates one captured source tuple before
// committing preparation. Success does not assert a runtime mode change.
type EdgeDeliveryPreparationExecutor interface {
	PrepareDelivery(context.Context, domain.EdgeCommandAuthority, domain.ProbeDeliveryPreparation) (domain.ProbeCommandOutcome, error)
}
