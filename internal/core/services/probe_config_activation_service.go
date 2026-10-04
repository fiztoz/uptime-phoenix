package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeConfigActivationService coordinates validation and atomic activation
// for local configuration snapshots. It never executes checks or sends alerts.
type LocalProbeConfigActivationService struct {
	validation *LocalProbeConfigValidationService
	activation ports.ProbeConfigActivationRepository
}

// NewLocalProbeConfigActivationService creates an activation service.
func NewLocalProbeConfigActivationService(validation *LocalProbeConfigValidationService, activation ports.ProbeConfigActivationRepository) *LocalProbeConfigActivationService {
	return &LocalProbeConfigActivationService{
		validation: validation,
		activation: activation,
	}
}

// Activate validates an exact prepared local revision, verifies source freshness
// under the activation transaction, checks expected active state, and persists
// the active pointer and receipt together.
func (s *LocalProbeConfigActivationService) Activate(
	ctx context.Context,
	target domain.ProbeConfigTarget,
	revision int64,
	expectedHash string,
	expectedActiveRevision int64,
) (*domain.ProbeActiveConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.validation == nil || s.activation == nil ||
		target.ProbeID != domain.LocalProbeID || !domain.ValidProbeConfigTarget(target) ||
		revision <= 0 || !domain.ValidKeyHash(expectedHash) || expectedActiveRevision < 0 {
		return nil, domain.ErrValidation
	}

	// 1. Validate the prepared snapshot semantically.
	metadata, err := s.validation.ValidatePrepared(ctx, target, revision)
	if err != nil {
		return nil, err
	}
	if metadata.SHA256 != expectedHash {
		return nil, ports.ErrConflict
	}

	// 2. Perform atomic activation with source freshness recheck.
	params := ports.LocalActivationParams{
		Target:                 target,
		Revision:               revision,
		SHA256:                 metadata.SHA256,
		ExpectedActiveRevision: expectedActiveRevision,
		AppliedAt:              time.Now().UTC(),
		AssignmentCount:        -1, // Determined from verified definition in transaction.
	}
	return s.activation.ActivateLocal(ctx, params)
}

// GetActive returns the current active configuration pointer for probeID.
func (s *LocalProbeConfigActivationService) GetActive(ctx context.Context, probeID string) (*domain.ProbeActiveConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.activation == nil || probeID == "" {
		return nil, domain.ErrValidation
	}
	return s.activation.GetActive(ctx, probeID)
}

// GetReceipt returns the durable receipt for probeID and revision.
func (s *LocalProbeConfigActivationService) GetReceipt(ctx context.Context, probeID string, revision int64) (*domain.ProbeActiveConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.activation == nil || probeID == "" || revision <= 0 {
		return nil, domain.ErrValidation
	}
	return s.activation.GetReceipt(ctx, probeID, revision)
}
