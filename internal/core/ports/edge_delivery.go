package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EdgeDeliveryRepository adds atomic source/config/attempt authorization to the
// outbox. A nil authorized item means the intent was superseded. ACK committed
// before authorization suppresses DOWN; already authorized I/O may complete.
type EdgeDeliveryRepository interface {
	DeliveryOutboxRepository
	AuthorizeEdgeDelivery(context.Context, domain.DeliveryClaim, domain.ProbeConfigMetadata, time.Duration) (*domain.QueuedDelivery, error)
}
