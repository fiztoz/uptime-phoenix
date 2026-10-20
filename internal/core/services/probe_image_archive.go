package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeImageArchiveService packs one borrowed private image snapshot into a
// sealed selected-layout archive. It validates the caller before any image or
// packer method, observes the entry verification exactly once, and disposes
// every partial, refused, malformed, foreign or interrupted output archive.
// The service itself never closes or opens the borrowed image. A local archive
// proves no daemon-load compatibility, publisher identity or remote authority.
type ProbeImageArchiveService struct {
	packer ports.ProbeImageArchivePacker
}

// probeImageArchiveSafeErrors is the complete bare allowlist for adapter
// diagnostics: the eight image errors, three snapshot errors, four archive
// errors, and the two context sentinels. It intentionally does not mutate
// probeImageSnapshotSafeErrors; archive sentinels must not collapse into
// ErrProbeImageUnavailable, and context sentinels stay last so a joined
// fixed cause is preferred when the caller context itself is still active.
var probeImageArchiveSafeErrors = []error{
	domain.ErrProbeImageInput,
	domain.ErrProbeImageSize,
	domain.ErrProbeImageDigest,
	domain.ErrProbeImageMetadata,
	domain.ErrProbeImageUnavailable,
	domain.ErrProbeImageContentLimit,
	domain.ErrProbeImageContentEncoding,
	domain.ErrProbeImageContentDiffID,
	domain.ErrProbeImageSnapshotStorage,
	domain.ErrProbeImageSnapshotClosed,
	domain.ErrProbeImageSnapshotBusy,
	domain.ErrProbeImageArchiveStorage,
	domain.ErrProbeImageArchiveClosed,
	domain.ErrProbeImageArchiveBusy,
	domain.ErrProbeImageArchiveIntegrity,
	context.Canceled,
	context.DeadlineExceeded,
}

// NewProbeImageArchiveService constructs the bounded offline archive use case.
// A nil or typed-nil packer is refused so no method is ever called on an
// absent adapter.
func NewProbeImageArchiveService(p ports.ProbeImageArchivePacker) (*ProbeImageArchiveService, error) {
	if probeImageSnapshotNil(p) {
		return nil, domain.ErrProbeImageInput
	}
	return &ProbeImageArchiveService{packer: p}, nil
}

// probeImageArchiveSafeError converts adapter diagnostics into a bare fixed
// error from the allowlist. Unknown and private diagnostics never escape.
func probeImageArchiveSafeError(err error) error {
	for _, safe := range probeImageArchiveSafeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeImageUnavailable
}

// refuseProbeImageArchive disposes a refused output independently of caller
// cancellation and never returns that handle. Any cleanup failure becomes bare
// archive storage, even when the cause was cancellation or snapshot storage:
// returning only cancellation would hide retained private bytes with no handle.
// A confirmed archive or snapshot storage cause otherwise survives cancellation.
// A typed-nil archive is not closed.
func refuseProbeImageArchive(archive ports.PreparedProbeImageArchive, ctx context.Context, cause error) (ports.PreparedProbeImageArchive, error) {
	if !probeImageSnapshotNil(archive) && archive.Close(context.WithoutCancel(ctx)) != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if errors.Is(cause, domain.ErrProbeImageArchiveStorage) {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if errors.Is(cause, domain.ErrProbeImageSnapshotStorage) {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, probeImageArchiveSafeError(cause)
}

// Pack borrows image for one pack. It never calls image.Open or image.Close,
// including invalid requests, adapter failures and cancellation. Context, pin
// and a typed-nil image are refused before any method. A valid verification
// whose selection pin is exactly the caller pin is captured before packing;
// the caller context is checked again after that getter, after pack, and after
// Descriptor. The published descriptor must be valid and its Content must be
// exactly that entry receipt. Every partial, refused, foreign, malformed or
// canceled output is disposed with an independent cleanup context. An accepted
// archive is returned as the packer's handle; the caller closes both the
// archive and the borrowed image.
func (s *ProbeImageArchiveService) Pack(ctx context.Context, pin domain.ProbeImagePin, image ports.PreparedProbeImage) (ports.PreparedProbeImageArchive, error) {
	if probeImageSnapshotNil(ctx) || !pin.Valid() || probeImageSnapshotNil(image) {
		return nil, domain.ErrProbeImageInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry := image.Verification()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !entry.Valid() || entry.Selection.Pin != pin {
		return nil, domain.ErrProbeImageMetadata
	}
	archive, err := s.packer.PackImageArchive(ctx, pin, image)
	if err != nil {
		return refuseProbeImageArchive(archive, ctx, err)
	}
	if err := ctx.Err(); err != nil {
		return refuseProbeImageArchive(archive, ctx, err)
	}
	if probeImageSnapshotNil(archive) {
		return refuseProbeImageArchive(archive, ctx, domain.ErrProbeImageMetadata)
	}
	descriptor := archive.Descriptor()
	if err := ctx.Err(); err != nil {
		return refuseProbeImageArchive(archive, ctx, err)
	}
	if !descriptor.Valid() || descriptor.Content != entry {
		return refuseProbeImageArchive(archive, ctx, domain.ErrProbeImageArchiveIntegrity)
	}
	return archive, nil
}
