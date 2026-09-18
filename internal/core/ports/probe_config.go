package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeConfigRepository stores prepared snapshots; Latest never means activated.
// Save compares expectedRevision with the latest retained revision (zero if none).
// A retry of the latest identical metadata returns the original stored ciphertext.
// Older revisions, conflicting content and authority changes fail with ErrConflict.
type ProbeConfigRepository interface {
	Save(ctx context.Context, snapshot domain.ProtectedProbeConfig, expectedRevision int64) (*domain.ProtectedProbeConfig, error)
	Get(ctx context.Context, probeID string, revision int64) (*domain.ProtectedProbeConfig, error)
	Latest(ctx context.Context, probeID string) (*domain.ProtectedProbeConfig, error)
}

// ProbeConfigInspector validates a complete bounded document and its references.
// It does not run extension validators, authorize assignments or activate work.
type ProbeConfigInspector interface {
	Inspect(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error)
}

// ProbeConfigProtector uses authenticated encryption bound to immutable metadata.
// Errors must not contain plaintext, key material or raw confidential documents.
type ProbeConfigProtector interface {
	Seal(ctx context.Context, metadata domain.ProbeConfigMetadata, plaintext []byte) ([]byte, error)
	Open(ctx context.Context, metadata domain.ProbeConfigMetadata, ciphertext []byte) ([]byte, error)
}
