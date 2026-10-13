package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AggregateStandbyService runs durable planning, never a provider or cutover.
type AggregateStandbyService struct {
	store ports.AggregateStandbyRepository
	owner string
}

// NewAggregateStandbyService constructs a hub standby planner.
func NewAggregateStandbyService(store ports.AggregateStandbyRepository, owner string) *AggregateStandbyService {
	return &AggregateStandbyService{store: store, owner: owner}
}

// EvaluateStandby reuses the pure availability decision without adding authority.
func (s *AggregateStandbyService) EvaluateStandby(in domain.AggregatePagingInput) (domain.AggregatePagingDecision, error) {
	return EvaluateAggregatePaging(in)
}

// ProcessBatch claims bounded work and reconciles each monitor independently.
func (s *AggregateStandbyService) ProcessBatch(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s == nil || s.store == nil {
		return 0, domain.ErrValidation
	}
	leases, err := s.store.ClaimStandby(ctx, s.owner, 30*time.Second, 20)
	n := 0
	// Claims may commit independently even when another monitor failed. Never
	// discard those live claims merely because the batch carries a typed error.
	failed := err
	for _, lease := range leases {
		if err := ctx.Err(); err != nil {
			return n, errors.Join(failed, err)
		}
		if _, err := s.store.ReconcileStandby(ctx, lease, s); err != nil {
			failed = errors.Join(failed, fmt.Errorf("aggregate standby reconciliation: %w", err))
			continue
		}
		n++
	}
	return n, failed
}

// Run reconciles periodically and exits only after its active call is joined.
// Report receives redacted repository errors; no providers are invoked.
func (s *AggregateStandbyService) Run(ctx context.Context, interval time.Duration, report func(error)) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.ProcessBatch(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
	}
}
