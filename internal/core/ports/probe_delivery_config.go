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
