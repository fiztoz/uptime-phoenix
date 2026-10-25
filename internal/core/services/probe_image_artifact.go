package services

import (
	"context"
	"errors"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeImagePreparationService validates explicit image pins and adapter results.
type ProbeImagePreparationService struct{ preparer ports.ProbeImagePreparer }

// NewProbeImagePreparationService constructs an offline metadata use case.
func NewProbeImagePreparationService(p ports.ProbeImagePreparer) (*ProbeImagePreparationService, error) {
	if p == nil {
		return nil, domain.ErrProbeImageInput
	}
	return &ProbeImagePreparationService{preparer: p}, nil
}

// Prepare consumes all streams and returns a digest-bound platform selection.
func (s *ProbeImagePreparationService) Prepare(ctx context.Context, pin domain.ProbeImagePin, in ports.ProbeImageInputs) (*domain.ProbeImageSelection, error) {
	closeInputs := func() {
		for _, r := range []io.ReadCloser{in.Index, in.Manifest, in.Config} {
			if r != nil {
				_ = r.Close()
			}
		}
	}
	if ctx == nil || !pin.Valid() || in.Index == nil || in.Manifest == nil || in.Config == nil {
		closeInputs()
		return nil, domain.ErrProbeImageInput
	}
	if ctx.Err() != nil {
		closeInputs()
		return nil, ctx.Err()
	}
	result, err := s.preparer.PrepareImage(ctx, pin, in)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		for _, safe := range []error{domain.ErrProbeImageInput, domain.ErrProbeImageSize, domain.ErrProbeImageDigest, domain.ErrProbeImageMetadata, domain.ErrProbeImageUnavailable, context.Canceled, context.DeadlineExceeded} {
			if errors.Is(err, safe) {
				return nil, safe
			}
		}
		return nil, domain.ErrProbeImageUnavailable
	}
	if result == nil || !result.Valid() || result.Pin != pin {
		return nil, domain.ErrProbeImageMetadata
	}
	copy := *result
	return &copy, nil
}
