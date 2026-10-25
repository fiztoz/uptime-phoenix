package services

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeBinaryPreparationService validates explicit pins before adapter I/O.
type ProbeBinaryPreparationService struct{ preparer ports.ProbeBinaryPreparer }

// NewProbeBinaryPreparationService constructs an offline preparation use case.
func NewProbeBinaryPreparationService(p ports.ProbeBinaryPreparer) (*ProbeBinaryPreparationService, error) {
	if p == nil {
		return nil, domain.ErrProbeArtifactInput
	}
	return &ProbeBinaryPreparationService{preparer: p}, nil
}

// Prepare consumes input and returns only a verified snapshot with the exact pin.
// It does not infer host compatibility, installation, enrollment or activation.
func (s *ProbeBinaryPreparationService) Prepare(ctx context.Context, pin domain.ProbeBinaryPin, input io.ReadCloser) (ports.PreparedProbeBinary, error) {
	if ctx == nil || input == nil || !pin.Valid() {
		if input != nil {
			_ = input.Close()
		}
		return nil, domain.ErrProbeArtifactInput
	}
	if err := ctx.Err(); err != nil {
		_ = input.Close()
		return nil, err
	}
	prepared, err := s.preparer.PrepareBinary(ctx, pin, input)
	if err != nil {
		if prepared != nil {
			_ = prepared.Close(context.WithoutCancel(ctx))
		}
		return nil, err
	}
	if prepared == nil {
		return nil, domain.ErrProbeArtifactMetadata
	}
	if prepared.Pin() != pin || ctx.Err() != nil {
		_ = prepared.Close(context.WithoutCancel(ctx))
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrProbeArtifactMetadata
	}
	return prepared, nil
}
