package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// AggregateStandbyEvaluator is a pure proposal callback inside a storage transaction.
// Implementations must perform no I/O or mutate borrowed evidence.
type AggregateStandbyEvaluator interface {
	EvaluateStandby(domain.AggregatePagingInput) (domain.AggregatePagingDecision, error)
}

// AggregateStandbyRepository persists planning only; it has no send/claim-delivery API.
type AggregateStandbyRepository interface {
	// ClaimStandby may return committed independent claims AND a redacted error
	// for other monitors. Callers must process those claims despite the error.
	ClaimStandby(context.Context, string, time.Duration, int) ([]domain.AggregateStandbyLease, error)
	ReconcileStandby(context.Context, domain.AggregateStandbyLease, AggregateStandbyEvaluator) (domain.AggregateStandbyResult, error)
}
