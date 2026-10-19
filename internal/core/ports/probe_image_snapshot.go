package ports

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeImageSnapshotPreparer owns all inputs, including refused requests, and
// returns only private copied bytes reverified against the exact original pin.
// It does not fetch, export an OCI layout, load, execute or authorize installation.
type ProbeImageSnapshotPreparer interface {
	PrepareImageSnapshot(ctx context.Context, pin domain.ProbeImagePin, inputs ProbeImageContentInputs) (PreparedProbeImage, error)
}

// PreparedProbeImage owns a private verified metadata/ordered-layer snapshot.
// Open allows one active bundle; each reader follows its Open context. Callers
// close every returned reader and eventually the snapshot. Close revokes/joins
// readers and attempts owned cleanup even with a nil/canceled cleanup context.
// No paths, mutable bytes, durable journal or remote authority are exposed.
type PreparedProbeImage interface {
	Verification() domain.ProbeImageContentVerification
	Open(ctx context.Context) (ProbeImageContentInputs, error)
	Close(ctx context.Context) error
}
