package services

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeImageSnapshotService validates caller input before any adapter I/O and
// accepts only a prepared snapshot whose verification is valid and bound to
// the exact original pin. It captures the layer list at entry, transfers stream
// ownership on entry (including refused requests), and disposes every partial,
// refused, malformed, foreign or interrupted snapshot. It retains no bytes,
// readers, local paths or storage diagnostics, and proves no export, load,
// execution, installation or remote authority.
type ProbeImageSnapshotService struct {
	preparer ports.ProbeImageSnapshotPreparer
}

// probeImageSnapshotSafeErrors is the complete allowlist of bare errors this
// service may return for adapter diagnostics: the eight existing image errors,
// the three snapshot errors, and context cancellation/deadline. Any other
// wrapped or private diagnostic becomes ErrProbeImageUnavailable.
var probeImageSnapshotSafeErrors = []error{
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
	context.Canceled,
	context.DeadlineExceeded,
}

// NewProbeImageSnapshotService constructs the bounded offline snapshot use
// case. A nil or typed-nil preparer is refused so no method is ever called on
// an absent adapter.
func NewProbeImageSnapshotService(p ports.ProbeImageSnapshotPreparer) (*ProbeImageSnapshotService, error) {
	if probeImageSnapshotNil(p) {
		return nil, domain.ErrProbeImageInput
	}
	return &ProbeImageSnapshotService{preparer: p}, nil
}

// probeImageSnapshotNil reports whether v is a nil interface or an interface
// holding a nil reference value. It never invokes methods on the value, so a
// typed-nil stream is refused without the panic its Close could raise.
func probeImageSnapshotNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}

// probeImageSnapshotComparable checks the actual value, including values held
// in nested interface fields; a comparable type alone is insufficient.
func probeImageSnapshotComparable(v any) bool {
	return v != nil && reflect.ValueOf(v).Comparable()
}

// probeImageSnapshotOwned enumerates the handles a closing owner may touch for
// the supplied slots. Typed-nil slots are refused without invoking methods.
// Comparable duplicate handles are refused and appear once in owned, so a
// duplicated handle is closed exactly once. Non-comparable values cannot be
// compared safely, so they are kept per supplied slot and closed per slot.
func probeImageSnapshotOwned(slots []io.ReadCloser) (owned []io.ReadCloser, ok bool) {
	ok = true
	seen := make(map[io.ReadCloser]struct{}, len(slots))
	for _, slot := range slots {
		if probeImageSnapshotNil(slot) {
			ok = false
			continue
		}
		if probeImageSnapshotComparable(slot) {
			if _, dup := seen[slot]; dup {
				ok = false
				continue
			}
			seen[slot] = struct{}{}
		}
		owned = append(owned, slot)
	}
	return owned, ok
}

// probeImageSnapshotSafeError converts adapter diagnostics into a bare fixed
// error from the allowlist; unknown and private diagnostics never escape.
func probeImageSnapshotSafeError(err error) error {
	for _, safe := range probeImageSnapshotSafeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeImageUnavailable
}

// refuseProbeImageSnapshot disposes a refused capability independently of
// caller cancellation. Confirmed cleanup/storage failure takes precedence:
// returning only cancellation would hide retained private storage with no handle.
func refuseProbeImageSnapshot(p ports.PreparedProbeImage, ctx context.Context, cause error) (ports.PreparedProbeImage, error) {
	if !probeImageSnapshotNil(p) && p.Close(context.WithoutCancel(ctx)) != nil {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	if errors.Is(cause, domain.ErrProbeImageSnapshotStorage) {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return nil, probeImageSnapshotSafeError(cause)
}

// Prepare consumes the supplied streams and returns only a private verified
// snapshot bound to the exact pin. Ownership of every distinct stream
// transfers on entry — including invalid, duplicate or canceled requests,
// which are closed without adapter I/O — and an accepted request transfers
// ownership exactly once to the preparer. The layer list is captured at entry,
// so later caller mutation cannot change what is closed or delegated. Any
// partial, refused, malformed, foreign or interrupted snapshot is disposed
// with an independent cleanup context before returning a bare fixed error.
func (s *ProbeImageSnapshotService) Prepare(ctx context.Context, pin domain.ProbeImagePin, inputs ports.ProbeImageContentInputs) (ports.PreparedProbeImage, error) {
	layers := append([]io.ReadCloser(nil), inputs.Layers...)
	slots := make([]io.ReadCloser, 0, 3+len(layers))
	slots = append(slots, inputs.Metadata.Index, inputs.Metadata.Manifest, inputs.Metadata.Config)
	slots = append(slots, layers...)
	owned, handlesOK := probeImageSnapshotOwned(slots)
	closeOwned := func() {
		for _, r := range owned {
			_ = r.Close()
		}
	}
	if probeImageSnapshotNil(ctx) || !pin.Valid() || !handlesOK ||
		len(layers) < 1 || len(layers) > domain.MaxProbeImageLayers {
		closeOwned()
		return nil, domain.ErrProbeImageInput
	}
	if err := ctx.Err(); err != nil {
		closeOwned()
		return nil, err
	}
	prepared, err := s.preparer.PrepareImageSnapshot(ctx, pin, ports.ProbeImageContentInputs{Metadata: inputs.Metadata, Layers: layers})
	if err != nil {
		return refuseProbeImageSnapshot(prepared, ctx, err)
	}
	if probeImageSnapshotNil(prepared) {
		return refuseProbeImageSnapshot(prepared, ctx, domain.ErrProbeImageMetadata)
	}
	if ctx.Err() != nil {
		return refuseProbeImageSnapshot(prepared, ctx, ctx.Err())
	}
	verification := prepared.Verification()
	if ctx.Err() != nil {
		return refuseProbeImageSnapshot(prepared, ctx, ctx.Err())
	}
	if !verification.Valid() || verification.Selection.Pin != pin {
		return refuseProbeImageSnapshot(prepared, ctx, domain.ErrProbeImageMetadata)
	}
	return prepared, nil
}
