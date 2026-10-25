package ports

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeImageArchivePacker borrows image without ever closing it. It owns and
// joins every stream returned by image.Open, including partial/error bundles,
// and returns only private sealed selected-layout tar bytes. No daemon, remote
// delivery, publisher/runtime identity or installation authority is established.
type ProbeImageArchivePacker interface {
	PackImageArchive(ctx context.Context, pin domain.ProbeImagePin, image PreparedProbeImage) (PreparedProbeImageArchive, error)
}

// PreparedProbeImageArchive owns a private uncompressed tar with one active
// context-bound reader. Close revokes/joins readers and attempts guarded owned
// cleanup even with nil/canceled context. Descriptor returns values, not paths,
// mutable bytes, transferable source inclusion proof or cleanup authority.
type PreparedProbeImageArchive interface {
	Descriptor() domain.ProbeImageArchiveDescriptor
	Open(ctx context.Context) (io.ReadCloser, error)
	Close(ctx context.Context) error
}
