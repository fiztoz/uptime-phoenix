package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// RemoteProbeDeliveryConfigReader projects exact receipt-backed remote documents
// into nonsecret assignment/mode facts. ErrNotFound means unreported; ErrConflict
// means the stored evidence cannot be verified. It never returns confidential
// settings, a document digest, or applied-mode/send/suppression authority.
type RemoteProbeDeliveryConfigReader interface {
	ReadAcknowledgedRemoteDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error)
}

// LocalProbeDeliveryConfigReader projects exact receipt-backed local documents.
// It does not rebuild current desired rows or expose confidential document data.
// It has the same non-authority and error contract as the remote reader.
type LocalProbeDeliveryConfigReader interface {
	ReadAcknowledgedLocalDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error)
}

// LocalProbeDeliveryConfigDecoder validates exact protected-document metadata
// and local extension semantics before projecting nonsecret assignment facts.
// It never performs checks, sends, persistence, activation or network I/O.
type LocalProbeDeliveryConfigDecoder interface {
	DecodeLocalDelivery(ctx context.Context, document []byte, expected domain.ProbeConfigMetadata) (*domain.AcknowledgedProbeDeliveryConfig, error)
}
