package services

import (
	"context"
	"errors"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeReplayService coordinates batch ingestion using the repository and authorizer.
type ProbeReplayService struct {
	repo       ports.ProbeReplayRepository
	authorizer ports.ProbeReplayAuthorizer
}

var _ ports.ProbeReplayService = (*ProbeReplayService)(nil)

// NewProbeReplayService constructs a new ProbeReplayService.
func NewProbeReplayService(repo ports.ProbeReplayRepository, authorizer ports.ProbeReplayAuthorizer) (*ProbeReplayService, error) {
	if repo == nil || authorizer == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeReplayService{
		repo:       repo,
		authorizer: authorizer,
	}, nil
}

// ProcessBatch coordinates transactional ingestion of a replayed batch.
func (s *ProbeReplayService) ProcessBatch(ctx context.Context, session domain.ProbeReplaySession, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
	if !domain.ValidProbeReplayBatch(session, batch) {
		return nil, fmt.Errorf("invalid replay framing: %w", domain.ErrValidation)
	}
	result, err := s.repo.IngestReplayBatch(ctx, session, batch, s.authorizer)
	if !errors.Is(err, domain.ErrInternal) || ctx.Err() != nil {
		return result, err
	}
	// A failed/ambiguous write is never an ACK. Read committed storage again;
	// if it is unavailable, close the session rather than inventing a cursor.
	cursor, readErr := s.repo.GetCursor(ctx, session.ProbeID, session.StreamID)
	if readErr != nil || cursor < batch.FirstSeq-1 {
		return nil, err
	}
	return &domain.ProbeReplayResult{StreamID: batch.StreamID, CommittedSeq: min(cursor, batch.LastSeq)}, domain.ErrReplayRetry
}
