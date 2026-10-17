package ports

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeImageInputs transfers ownership of three distinct streams on preparation.
// Each closer must unblock its corresponding pending Read.
type ProbeImageInputs struct {
	Index    io.ReadCloser
	Manifest io.ReadCloser
	Config   io.ReadCloser
}

// ProbeImagePreparer validates the exact index/manifest/config digest chain.
// It does not contact a registry/daemon, fetch layers or authorize installation.
type ProbeImagePreparer interface {
	PrepareImage(ctx context.Context, pin domain.ProbeImagePin, inputs ProbeImageInputs) (*domain.ProbeImageSelection, error)
}
