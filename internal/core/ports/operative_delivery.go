package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// DeliveryCutoverCodec maps a protected closed request to bounded pure data.
type DeliveryCutoverCodec interface {
	EncodeDeliveryCutover(context.Context, domain.DeliveryCutoverCommand) ([]byte, error)
	DecodeDeliveryCutover(context.Context, []byte) (domain.DeliveryCutoverCommand, error)
}

// EdgeDeliveryCutoverRepository atomically fences source gates and kind receipts.
// A nil capture permits historical receipt recovery/resume only; it cannot apply
// a new current configuration. Resume never clears a replacement barrier.
type EdgeDeliveryCutoverRepository interface {
	ReadDeliveryCutoverReceipt(context.Context, domain.EdgeCommandAuthority, domain.DeliveryCutoverCommand) (*domain.ProbeCommandOutcome, error)
	ApplyDeliveryCutover(context.Context, domain.EdgeCommandAuthority, domain.DeliveryCutoverCommand, *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error)
}

// EdgeDeliveryCutoverExecutor authenticates the one captured tuple used by storage.
type EdgeDeliveryCutoverExecutor interface {
	ExecuteDeliveryCutover(context.Context, domain.EdgeCommandAuthority, domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error)
}

// SourceProviderAttemptJournal completes only an exact admitted provider grant.
// Unlike new-send admission, a matching release may finish after lease expiry.
type SourceProviderAttemptJournal interface {
	FinishSourceProviderAttempt(context.Context, string, domain.ProviderAttemptState) error
}

// LocalDeliveryAuthorizer independently fences stored local claim, entire graph,
// incident, source gate and provider budget before committing an attempt grant.
type LocalDeliveryAuthorizer interface {
	AuthorizeLocalDelivery(context.Context, domain.DeliveryClaim, domain.ProbeActiveConfig, time.Duration) (*domain.LocalProviderGrant, error)
}

// OperativeSourceCapabilityRecorder fences handshake capability evidence by the
// accepted database session. A stale owner cannot assert or withdraw support.
type OperativeSourceCapabilityRecorder interface {
	SetOperativeSourceCapability(context.Context, domain.ProbeReplaySession, bool) error
}

// AggregateCutoverEvaluator is pure and confers no database authority.
type AggregateCutoverEvaluator interface {
	EvaluateAggregate(domain.AggregatePagingInput) (domain.AggregatePagingDecision, error)
	EvaluateCutover(domain.DeliveryCutoverInput) (domain.DeliveryCutoverDecision, error)
}

// AggregateCutoverRepository coordinates full barriers under independent claims.
// Claims may be returned alongside per-monitor typed errors; they are committed
// and must be processed. enabled=false drains existing work but cannot activate.
type AggregateCutoverRepository interface {
	ClaimCutovers(context.Context, string, time.Duration, int) ([]domain.AggregateStandbyLease, error)
	ReconcileCutover(context.Context, domain.AggregateStandbyLease, bool, AggregateCutoverEvaluator) (domain.AggregateCutoverProgress, error)
}

// AggregateDeliveryRepository persists genuine hub work separately from standby.
// The returned provider grant comes from one independently fenced capture.
type AggregateDeliveryRepository interface {
	ClaimAggregateDelivery(context.Context, string, time.Duration) (*domain.QueuedDelivery, error)
	AuthorizeAggregateDelivery(context.Context, domain.DeliveryClaim, time.Duration) (*domain.AggregateProviderGrant, error)
	FinishAggregateDelivery(context.Context, domain.DeliveryClaim, domain.DeliveryResult) error
}
