package ports

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeImageContentInputs transfers ownership of all distinct streams on entry,
// including invalid inputs. Layers are supplied in exact manifest order. Each
// closer must unblock its pending Read; callers may not mutate the stream list
// concurrently. No stream, snapshot or future transfer authority is returned.
type ProbeImageContentInputs struct {
	Metadata ProbeImageInputs
	Layers   []io.ReadCloser
}

// ProbeImageContentVerifier checks the metadata chain, compressed layer hashes
// and sizes, and bounded uncompressed diff IDs without extracting or executing.
type ProbeImageContentVerifier interface {
	VerifyImageContent(ctx context.Context, pin domain.ProbeImagePin, inputs ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error)
}
