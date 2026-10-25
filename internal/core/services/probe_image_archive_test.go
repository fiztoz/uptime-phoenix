package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// imageArchivePanicImage is a typed-nil trap: any method call panics.
type imageArchivePanicImage struct{}

func (*imageArchivePanicImage) Verification() domain.ProbeImageContentVerification {
	panic("typed-nil image touched")
}
func (*imageArchivePanicImage) Open(context.Context) (ports.ProbeImageContentInputs, error) {
	panic("typed-nil image touched")
}
func (*imageArchivePanicImage) Close(context.Context) error { panic("typed-nil image touched") }

// imageArchiveImage records the single entry getter. Open and Close panic so
// a passing test proves the borrowed snapshot is never taken or released.
type imageArchiveImage struct {
	verif    domain.ProbeImageContentVerification
	verifies int
	onVerify func()
}

func (m *imageArchiveImage) Verification() domain.ProbeImageContentVerification {
	m.verifies++
	if m.onVerify != nil {
		m.onVerify()
	}
	return m.verif
}

func (m *imageArchiveImage) Open(context.Context) (ports.ProbeImageContentInputs, error) {
	panic("borrowed image opened")
}
func (m *imageArchiveImage) Close(context.Context) error { panic("borrowed image closed") }

// imageArchivePanicArchive is a typed-nil output trap.
type imageArchivePanicArchive struct{}

func (*imageArchivePanicArchive) Descriptor() domain.ProbeImageArchiveDescriptor {
	panic("typed-nil archive touched")
}
func (*imageArchivePanicArchive) Open(context.Context) (io.ReadCloser, error) {
	panic("typed-nil archive touched")
}
func (*imageArchivePanicArchive) Close(context.Context) error { panic("typed-nil archive touched") }

// imageArchivePrepared records descriptor access and independent disposal.
// Open panics: the service must not read archive bytes.
type imageArchivePrepared struct {
	desc       domain.ProbeImageArchiveDescriptor
	descCalls  int
	closeCalls int
	closeCtx   context.Context
	closeErr   error
	onDesc     func()
}

func (a *imageArchivePrepared) Descriptor() domain.ProbeImageArchiveDescriptor {
	a.descCalls++
	if a.onDesc != nil {
		a.onDesc()
	}
	return a.desc
}

func (a *imageArchivePrepared) Open(context.Context) (io.ReadCloser, error) {
	panic("service opened archive")
}

func (a *imageArchivePrepared) Close(ctx context.Context) error {
	a.closeCalls++
	a.closeCtx = ctx
	return a.closeErr
}

// imageArchivePacker records the single delegated pack.
type imageArchivePacker struct {
	calls  int
	pin    domain.ProbeImagePin
	image  ports.PreparedProbeImage
	result ports.PreparedProbeImageArchive
	err    error
	onCall func()
}

func (p *imageArchivePacker) PackImageArchive(_ context.Context, pin domain.ProbeImagePin, image ports.PreparedProbeImage) (ports.PreparedProbeImageArchive, error) {
	p.calls++
	p.pin = pin
	p.image = image
	if p.onCall != nil {
		p.onCall()
	}
	return p.result, p.err
}

func imageArchiveDescriptor(content domain.ProbeImageContentVerification) domain.ProbeImageArchiveDescriptor {
	return domain.ProbeImageArchiveDescriptor{
		Content:           content,
		LayoutIndexSHA256: strings.Repeat("d", 64),
		LayoutIndexSize:   128,
		ArchiveSHA256:     strings.Repeat("e", 64),
		ArchiveSize:       1024,
	}
}

func assertImageArchiveDisposed(t *testing.T, archive *imageArchivePrepared) {
	t.Helper()
	if archive.closeCalls != 1 {
		t.Fatal("output archive not disposed exactly once", archive.closeCalls)
	}
	if archive.closeCtx == nil || archive.closeCtx.Err() != nil || archive.closeCtx.Done() != nil {
		t.Fatal("cleanup context not independent of cancellation")
	}
}

func assertBareArchiveError(t *testing.T, err, want error) {
	t.Helper()
	if err != want {
		t.Fatal("error not the bare fixed sentinel", err)
	}
	if err != nil && strings.Contains(err.Error(), "sensitive-") {
		t.Fatal("adapter diagnostics leaked")
	}
}

func TestProbeImageArchiveServiceConstructor(t *testing.T) {
	if svc, err := NewProbeImageArchiveService(nil); svc != nil || err != domain.ErrProbeImageInput {
		t.Fatal("nil packer accepted", err)
	}
	var typedNil *imageArchivePacker
	if svc, err := NewProbeImageArchiveService(typedNil); svc != nil || err != domain.ErrProbeImageInput {
		t.Fatal("typed-nil packer accepted", err)
	}
	var iface ports.ProbeImageArchivePacker = (*imageArchivePacker)(nil)
	if svc, err := NewProbeImageArchiveService(iface); svc != nil || err != domain.ErrProbeImageInput {
		t.Fatal("typed-nil packer interface accepted", err)
	}
	if _, err := NewProbeImageArchiveService(&imageArchivePacker{}); err != nil {
		t.Fatal("valid packer refused", err)
	}
}

func TestProbeImageArchiveServiceRefusesBeforeIO(t *testing.T) {
	pin := imageSnapshotPin()
	kinds := []string{"nil_context", "typed_nil_context", "invalid_pin", "nil_image", "typed_nil_image", "already_canceled", "already_expired"}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			adapter := &imageArchivePacker{}
			svc, err := NewProbeImageArchiveService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			image := &imageArchiveImage{verif: imageSnapshotVerification(pin)}
			var callImage ports.PreparedProbeImage = image
			callCtx := ctx
			request := pin
			want := domain.ErrProbeImageInput
			switch kind {
			case "nil_context":
				callCtx = nil
				callImage = &imageArchivePanicImage{}
			case "typed_nil_context":
				var nilCtx *imageSnapshotPanicContext
				callCtx = nilCtx
				callImage = &imageArchivePanicImage{}
			case "invalid_pin":
				request.Version = "latest"
				callImage = &imageArchivePanicImage{}
			case "nil_image":
				callImage = nil
			case "typed_nil_image":
				callImage = (*imageArchivePanicImage)(nil)
			case "already_canceled":
				cancel()
				callImage = &imageArchivePanicImage{}
				want = context.Canceled
			case "already_expired":
				var stop context.CancelFunc
				callCtx, stop = context.WithDeadline(t.Context(), time.Unix(0, 0))
				defer stop()
				callImage = &imageArchivePanicImage{}
				want = context.DeadlineExceeded
			}
			got, err := svc.Pack(callCtx, request, callImage)
			if got != nil {
				t.Fatal("refused request returned an archive")
			}
			assertBareArchiveError(t, err, want)
			if adapter.calls != 0 {
				t.Fatal("refused request packed")
			}
			if image.verifies != 0 {
				t.Fatal("image method called before validation")
			}
		})
	}
}

func TestProbeImageArchiveServiceEntryReceipt(t *testing.T) {
	pin := imageSnapshotPin()
	cases := []struct {
		kind string
		want error
	}{
		{kind: "invalid", want: domain.ErrProbeImageMetadata},
		{kind: "foreign_pin", want: domain.ErrProbeImageMetadata},
		{kind: "canceled_during_getter", want: context.Canceled},
		{kind: "canceled_and_invalid", want: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			image := &imageArchiveImage{verif: imageSnapshotVerification(pin)}
			adapter := &imageArchivePacker{result: &imageArchivePrepared{desc: imageArchiveDescriptor(image.verif)}}
			switch tc.kind {
			case "invalid":
				image.verif = domain.ProbeImageContentVerification{}
			case "foreign_pin":
				foreign := pin
				foreign.Architecture = "arm64"
				image.verif = imageSnapshotVerification(foreign)
			case "canceled_during_getter":
				image.onVerify = cancel
			case "canceled_and_invalid":
				image.verif = domain.ProbeImageContentVerification{}
				image.onVerify = cancel
			}
			svc, err := NewProbeImageArchiveService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Pack(ctx, pin, image)
			if got != nil {
				t.Fatal("bad entry receipt packed through")
			}
			assertBareArchiveError(t, err, tc.want)
			if image.verifies != 1 || adapter.calls != 0 {
				t.Fatal("entry receipt not observed exactly once before pack", image.verifies, adapter.calls)
			}
		})
	}
}

func TestProbeImageArchiveServiceDelegation(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	archive := &imageArchivePrepared{desc: imageArchiveDescriptor(entry)}
	image := &imageArchiveImage{verif: entry}
	adapter := &imageArchivePacker{result: archive}
	svc, err := NewProbeImageArchiveService(adapter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Pack(t.Context(), pin, image)
	if err != nil || got != ports.PreparedProbeImageArchive(archive) {
		t.Fatal("exact archive refused", err)
	}
	if adapter.calls != 1 || adapter.pin != pin || adapter.image != ports.PreparedProbeImage(image) {
		t.Fatal("not packed exactly once with the borrowed image and pin")
	}
	if image.verifies != 1 || archive.descCalls != 1 || archive.closeCalls != 0 {
		t.Fatal("accepted archive lifecycle wrong", image.verifies, archive.descCalls, archive.closeCalls)
	}
	if got.Descriptor() != archive.desc || archive.desc.Content != entry {
		t.Fatal("returned receipt is not the entry receipt")
	}
	if archive.desc.LayoutIndexSHA256 == pin.IndexSHA256 {
		t.Fatal("test no longer distinguishes layout root from original index")
	}
}

func TestProbeImageArchiveServiceCoincidentLayoutRoot(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	desc := imageArchiveDescriptor(entry)
	desc.LayoutIndexSHA256 = pin.IndexSHA256
	desc.LayoutIndexSize = pin.IndexSize
	archive := &imageArchivePrepared{desc: desc}
	image := &imageArchiveImage{verif: entry}
	adapter := &imageArchivePacker{result: archive}
	svc, err := NewProbeImageArchiveService(adapter)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Pack(t.Context(), pin, image)
	if err != nil || got != ports.PreparedProbeImageArchive(archive) || archive.closeCalls != 0 {
		t.Fatal("coincident layout root refused", err)
	}
}

func TestProbeImageArchiveServiceResults(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	cases := []struct {
		kind     string
		want     error
		disposed bool
	}{
		{kind: "nil_result", want: domain.ErrProbeImageMetadata},
		{kind: "typed_nil_result", want: domain.ErrProbeImageMetadata},
		{kind: "typed_nil_with_error", want: domain.ErrProbeImageUnavailable},
		{kind: "content_drift", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "foreign_content", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "unaligned_size", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "below_min_size", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "above_max_size", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "bad_layout_size", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "uppercase_digest", want: domain.ErrProbeImageArchiveIntegrity, disposed: true},
		{kind: "partial_private", want: domain.ErrProbeImageUnavailable, disposed: true},
		{kind: "partial_wrapped", want: domain.ErrProbeImageArchiveBusy, disposed: true},
		{kind: "max_size", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			desc := imageArchiveDescriptor(entry)
			archive := &imageArchivePrepared{desc: desc}
			image := &imageArchiveImage{verif: entry}
			adapter := &imageArchivePacker{result: archive}
			switch tc.kind {
			case "nil_result":
				adapter.result = nil
			case "typed_nil_result":
				adapter.result = (*imageArchivePanicArchive)(nil)
			case "typed_nil_with_error":
				adapter.result = (*imageArchivePanicArchive)(nil)
				adapter.err = errors.New("sensitive-private-/var/lib/path")
			case "content_drift":
				drifted := entry
				drifted.LayerBytes++
				archive.desc.Content = drifted
			case "foreign_content":
				foreign := pin
				foreign.Architecture = "arm64"
				archive.desc.Content = imageSnapshotVerification(foreign)
			case "unaligned_size":
				archive.desc.ArchiveSize = 1025
			case "below_min_size":
				archive.desc.ArchiveSize = 512
			case "above_max_size":
				archive.desc.ArchiveSize = domain.MaxProbeImageArchiveBytes + 512
			case "bad_layout_size":
				archive.desc.LayoutIndexSize = domain.MaxProbeImageMetadataSize + 1
			case "uppercase_digest":
				archive.desc.ArchiveSHA256 = strings.ToUpper(archive.desc.ArchiveSHA256)
			case "partial_private":
				adapter.err = errors.New("sensitive-private-/var/lib/path")
			case "partial_wrapped":
				adapter.err = fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageArchiveBusy)
			case "max_size":
				archive.desc.ArchiveSize = domain.MaxProbeImageArchiveBytes
			}
			svc, err := NewProbeImageArchiveService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Pack(t.Context(), pin, image)
			if adapter.calls != 1 || image.verifies != 1 {
				t.Fatal("pack was retried or entry receipt reread", adapter.calls, image.verifies)
			}
			if tc.want == nil {
				if err != nil || got != ports.PreparedProbeImageArchive(archive) || archive.closeCalls != 0 {
					t.Fatal("boundary-valid archive refused", err)
				}
				return
			}
			if got != nil {
				t.Fatal("refused archive returned")
			}
			assertBareArchiveError(t, err, tc.want)
			if tc.disposed {
				assertImageArchiveDisposed(t, archive)
			} else if archive.closeCalls != 0 {
				t.Fatal("absent archive disposed")
			}
		})
	}
}

func TestProbeImageArchiveServicePostObservationMutation(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	mutated := entry
	mutated.UncompressedBytes++
	t.Run("entry_receipt_sticks", func(t *testing.T) {
		image := &imageArchiveImage{verif: entry}
		archive := &imageArchivePrepared{desc: imageArchiveDescriptor(entry)}
		adapter := &imageArchivePacker{result: archive, onCall: func() { image.verif = mutated }}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(t.Context(), pin, image)
		if err != nil || got != ports.PreparedProbeImageArchive(archive) || archive.closeCalls != 0 {
			t.Fatal("later image mutation replaced the entry receipt", err)
		}
		if image.verifies != 1 {
			t.Fatal("verification reread after pack")
		}
	})
	t.Run("mutated_descriptor_refused", func(t *testing.T) {
		image := &imageArchiveImage{verif: entry}
		archive := &imageArchivePrepared{desc: imageArchiveDescriptor(mutated)}
		adapter := &imageArchivePacker{result: archive, onCall: func() { image.verif = mutated }}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(t.Context(), pin, image)
		if got != nil {
			t.Fatal("descriptor matching a post-pack mutation accepted")
		}
		assertBareArchiveError(t, err, domain.ErrProbeImageArchiveIntegrity)
		assertImageArchiveDisposed(t, archive)
		if adapter.calls != 1 || image.verifies != 1 {
			t.Fatal("mismatch retried or reread", adapter.calls, image.verifies)
		}
	})
}

func TestProbeImageArchiveServiceErrorAllowlist(t *testing.T) {
	pin := imageSnapshotPin()
	cases := []struct {
		kind string
		err  error
		want error
	}{
		{kind: "input", err: errors.Join(domain.ErrProbeImageInput, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageInput},
		{kind: "size", err: errors.Join(domain.ErrProbeImageSize, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageSize},
		{kind: "digest", err: errors.Join(domain.ErrProbeImageDigest, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageDigest},
		{kind: "metadata", err: errors.Join(domain.ErrProbeImageMetadata, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageMetadata},
		{kind: "unavailable", err: errors.Join(domain.ErrProbeImageUnavailable, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageUnavailable},
		{kind: "content_limit", err: errors.Join(domain.ErrProbeImageContentLimit, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageContentLimit},
		{kind: "content_encoding", err: errors.Join(domain.ErrProbeImageContentEncoding, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageContentEncoding},
		{kind: "content_diffid", err: errors.Join(domain.ErrProbeImageContentDiffID, errors.New("sensitive-adapter-marker")), want: domain.ErrProbeImageContentDiffID},
		{kind: "snapshot_storage", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageSnapshotStorage), want: domain.ErrProbeImageSnapshotStorage},
		{kind: "snapshot_closed", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageSnapshotClosed), want: domain.ErrProbeImageSnapshotClosed},
		{kind: "snapshot_busy", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageSnapshotBusy), want: domain.ErrProbeImageSnapshotBusy},
		{kind: "archive_storage", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageArchiveStorage), want: domain.ErrProbeImageArchiveStorage},
		{kind: "archive_closed", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageArchiveClosed), want: domain.ErrProbeImageArchiveClosed},
		{kind: "archive_busy", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageArchiveBusy), want: domain.ErrProbeImageArchiveBusy},
		{kind: "archive_integrity", err: fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageArchiveIntegrity), want: domain.ErrProbeImageArchiveIntegrity},
		{kind: "canceled", err: errors.Join(context.Canceled, errors.New("sensitive-adapter-marker")), want: context.Canceled},
		{kind: "deadline", err: errors.Join(context.DeadlineExceeded, errors.New("sensitive-adapter-marker")), want: context.DeadlineExceeded},
		{kind: "private_error", err: errors.New("sensitive-private-/var/lib/path"), want: domain.ErrProbeImageUnavailable},
		{kind: "wrapped_private", err: fmt.Errorf("sensitive-private-/var/lib/path: %w", errors.New("inner-marker")), want: domain.ErrProbeImageUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			image := &imageArchiveImage{verif: imageSnapshotVerification(pin)}
			adapter := &imageArchivePacker{err: tc.err}
			svc, err := NewProbeImageArchiveService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Pack(t.Context(), pin, image)
			if got != nil {
				t.Fatal("failed pack returned an archive")
			}
			assertBareArchiveError(t, err, tc.want)
			if adapter.calls != 1 || image.verifies != 1 {
				t.Fatal("diagnostic path did not pack the observed entry once")
			}
		})
	}
}

func TestProbeImageArchiveServiceCleanupFailure(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	for _, kind := range []string{"private_error", "canceled_pack", "integrity", "canceled_descriptor", "snapshot_storage"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			archive := &imageArchivePrepared{desc: imageArchiveDescriptor(entry), closeErr: errors.New("sensitive-cleanup-marker")}
			image := &imageArchiveImage{verif: entry}
			adapter := &imageArchivePacker{result: archive}
			switch kind {
			case "private_error":
				adapter.err = errors.New("sensitive-private-/var/lib/path")
			case "canceled_pack":
				adapter.onCall = cancel
			case "integrity":
				archive.desc.ArchiveSize = 512
			case "canceled_descriptor":
				archive.onDesc = cancel
			case "snapshot_storage":
				adapter.err = domain.ErrProbeImageSnapshotStorage
				adapter.onCall = cancel
			}
			svc, err := NewProbeImageArchiveService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Pack(ctx, pin, image)
			if got != nil {
				t.Fatal("cleanup failure returned a handle")
			}
			assertBareArchiveError(t, err, domain.ErrProbeImageArchiveStorage)
			assertImageArchiveDisposed(t, archive)
			if adapter.calls != 1 {
				t.Fatal("cleanup failure retried pack", adapter.calls)
			}
		})
	}
}

func TestProbeImageArchiveServiceCancellation(t *testing.T) {
	pin := imageSnapshotPin()
	entry := imageSnapshotVerification(pin)
	t.Run("canceled_during_pack", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		archive := &imageArchivePrepared{desc: imageArchiveDescriptor(entry)}
		image := &imageArchiveImage{verif: entry}
		adapter := &imageArchivePacker{result: archive, onCall: cancel}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(ctx, pin, image)
		if got != nil {
			t.Fatal("canceled pack returned an archive")
		}
		assertBareArchiveError(t, err, context.Canceled)
		assertImageArchiveDisposed(t, archive)
		if archive.descCalls != 0 {
			t.Fatal("descriptor read after canceled pack")
		}
	})
	t.Run("canceled_during_descriptor", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		archive := &imageArchivePrepared{desc: domain.ProbeImageArchiveDescriptor{}, onDesc: cancel}
		image := &imageArchiveImage{verif: entry}
		adapter := &imageArchivePacker{result: archive}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(ctx, pin, image)
		if got != nil {
			t.Fatal("canceled descriptor returned an archive")
		}
		assertBareArchiveError(t, err, context.Canceled)
		assertImageArchiveDisposed(t, archive)
		if adapter.calls != 1 || archive.descCalls != 1 {
			t.Fatal("descriptor cancellation did not stop after one read")
		}
	})
	t.Run("archive_storage_survives", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		archive := &imageArchivePrepared{desc: imageArchiveDescriptor(entry)}
		image := &imageArchiveImage{verif: entry}
		adapter := &imageArchivePacker{
			result: archive,
			err:    errors.Join(domain.ErrProbeImageArchiveStorage, errors.New("sensitive-adapter-marker")),
			onCall: cancel,
		}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(ctx, pin, image)
		if got != nil {
			t.Fatal("storage failure returned a handle")
		}
		assertBareArchiveError(t, err, domain.ErrProbeImageArchiveStorage)
		assertImageArchiveDisposed(t, archive)
	})
	t.Run("snapshot_storage_survives", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		image := &imageArchiveImage{verif: entry}
		adapter := &imageArchivePacker{
			result: (*imageArchivePanicArchive)(nil),
			err:    fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageSnapshotStorage),
			onCall: cancel,
		}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(ctx, pin, image)
		if got != nil {
			t.Fatal("snapshot storage failure returned a handle")
		}
		assertBareArchiveError(t, err, domain.ErrProbeImageSnapshotStorage)
	})
	t.Run("nil_archive_storage_survives", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		image := &imageArchiveImage{verif: entry}
		adapter := &imageArchivePacker{err: domain.ErrProbeImageArchiveStorage, onCall: cancel}
		svc, err := NewProbeImageArchiveService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Pack(ctx, pin, image)
		if got != nil {
			t.Fatal("absent storage failure returned a handle")
		}
		assertBareArchiveError(t, err, domain.ErrProbeImageArchiveStorage)
	})
}
