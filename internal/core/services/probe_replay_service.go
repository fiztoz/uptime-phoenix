package services

import (
	"context"
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
	return s.repo.IngestReplayBatch(ctx, session, batch, s.authorizer)
}
