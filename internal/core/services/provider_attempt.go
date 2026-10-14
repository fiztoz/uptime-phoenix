package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// finishProviderAttempt runs only after Send returns. A provider error remains
// ambiguous; cancellation or lease expiry never manufactures completion.
func finishProviderAttempt(ctx context.Context, journal ports.SourceProviderAttemptJournal, id string, sendErr error) error {
	if id == "" {
		return nil
	}
	if journal == nil {
		return domain.ErrInternal
	}
	state := domain.ProviderAttemptSucceeded
	if sendErr != nil {
		state = domain.ProviderAttemptAmbiguous
	}
	return finishProviderAttemptState(ctx, journal, id, state)
}

// finishProviderAttemptState bounds the independent exact-grant completion.
func finishProviderAttemptState(ctx context.Context, journal ports.SourceProviderAttemptJournal, id string, state domain.ProviderAttemptState) error {
	if journal == nil || id == "" {
		return domain.ErrInternal
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return journal.FinishSourceProviderAttempt(releaseCtx, id, state)
}
