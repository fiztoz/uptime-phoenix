package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeConfigValidationService validates an exact protected local revision.
// It never records an applied receipt or grants scheduling/delivery authority.
type LocalProbeConfigValidationService struct {
	prepared  *ProbeConfigService
	validator ports.LocalProbeConfigValidator
}

// NewLocalProbeConfigValidationService requires authenticated snapshot reads and
// a validator wired to the installed runtime extensions.
func NewLocalProbeConfigValidationService(prepared *ProbeConfigService, validator ports.LocalProbeConfigValidator) *LocalProbeConfigValidationService {
	return &LocalProbeConfigValidationService{prepared: prepared, validator: validator}
}

// ValidatePrepared authenticates and validates an exact revision; zero (latest)
// is deliberately unsupported. Only metadata leaves this confidential boundary.
// The future activation transaction must recheck current source/assignment
// authority and bind this exact hash; validation is not a durable readiness claim.
func (s *LocalProbeConfigValidationService) ValidatePrepared(ctx context.Context, target domain.ProbeConfigTarget, revision int64) (domain.ProbeConfigMetadata, error) {
	if err := ctx.Err(); err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	if s == nil || s.prepared == nil || s.validator == nil || revision <= 0 ||
		target.ProbeID != domain.LocalProbeID || !domain.ValidProbeConfigTarget(target) {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	document, metadata, err := s.prepared.Read(ctx, target, revision)
	if err != nil {
		return domain.ProbeConfigMetadata{}, configBuildError(ctx, "read local configuration for validation", err)
	}
	if err := s.validator.ValidateLocal(ctx, document, target); err != nil {
		return domain.ProbeConfigMetadata{}, configBuildError(ctx, "validate local configuration", err)
	}
	if err := ctx.Err(); err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	return metadata, nil
}
