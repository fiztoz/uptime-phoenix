package services

import (
	"context"
	"errors"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeImageContentService validates supplied layer bytes against an already
// pinned metadata chain. It proves bounded content binding only and retains
// no bytes, readers or source paths.
type ProbeImageContentService struct {
	verifier ports.ProbeImageContentVerifier
}

// NewProbeImageContentService constructs the bounded offline content use case.
func NewProbeImageContentService(v ports.ProbeImageContentVerifier) (*ProbeImageContentService, error) {
	if v == nil {
		return nil, domain.ErrProbeImageInput
	}
	return &ProbeImageContentService{verifier: v}, nil
}

// Verify consumes every supplied stream and returns a copy of the digest-bound
// content verification. Ownership of all distinct streams transfers on entry,
// including refused input: refusal closes every stream exactly once, and an
// accepted request is closed by the verifier. The layer list is captured at
// entry, so later caller mutation cannot change what is closed or delegated.
func (s *ProbeImageContentService) Verify(ctx context.Context, pin domain.ProbeImagePin, inputs ports.ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error) {
	layers := append([]io.ReadCloser(nil), inputs.Layers...)
	closeInputs := func() {
		owned := append([]io.ReadCloser{inputs.Metadata.Index, inputs.Metadata.Manifest, inputs.Metadata.Config}, layers...)
		for _, r := range owned {
			if r != nil {
				_ = r.Close()
			}
		}
	}
	if ctx == nil || !pin.Valid() ||
		inputs.Metadata.Index == nil || inputs.Metadata.Manifest == nil || inputs.Metadata.Config == nil ||
		len(layers) < 1 || len(layers) > domain.MaxProbeImageLayers {
		closeInputs()
		return nil, domain.ErrProbeImageInput
	}
	for _, r := range layers {
		if r == nil {
			closeInputs()
			return nil, domain.ErrProbeImageInput
		}
	}
	if ctx.Err() != nil {
		closeInputs()
		return nil, ctx.Err()
	}
	result, err := s.verifier.VerifyImageContent(ctx, pin, ports.ProbeImageContentInputs{Metadata: inputs.Metadata, Layers: layers})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		for _, safe := range []error{
			domain.ErrProbeImageInput,
			domain.ErrProbeImageSize,
			domain.ErrProbeImageDigest,
			domain.ErrProbeImageMetadata,
			domain.ErrProbeImageUnavailable,
			domain.ErrProbeImageContentLimit,
			domain.ErrProbeImageContentEncoding,
			domain.ErrProbeImageContentDiffID,
			context.Canceled,
			context.DeadlineExceeded,
		} {
			if errors.Is(err, safe) {
				return nil, safe
			}
		}
		return nil, domain.ErrProbeImageUnavailable
	}
	if result == nil || !result.Valid() || result.Selection.Pin != pin {
		return nil, domain.ErrProbeImageMetadata
	}
	copy := *result
	return &copy, nil
}
