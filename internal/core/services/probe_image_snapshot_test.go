package services

import (
	"bytes"
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

// imageSnapshotInput counts closures so tests can assert exact ownership.
type imageSnapshotInput struct {
	io.Reader
	closed int
}

func (r *imageSnapshotInput) Close() error { r.closed++; return nil }

// imageSnapshotPanicHandle is a typed-nil trap: any method call panics, so a
// passing test proves typed-nil streams are refused without touching them.
type imageSnapshotPanicHandle struct{}

func (*imageSnapshotPanicHandle) Read([]byte) (int, error) { panic("typed-nil stream touched") }
func (*imageSnapshotPanicHandle) Close() error             { panic("typed-nil stream touched") }

// imageSnapshotPanicContext is a typed-nil context trap with the same
// guarantee for context methods.
type imageSnapshotPanicContext struct{}

func (*imageSnapshotPanicContext) Deadline() (deadline time.Time, ok bool) {
	panic("typed-nil context touched")
}
func (*imageSnapshotPanicContext) Done() <-chan struct{} { panic("typed-nil context touched") }
func (*imageSnapshotPanicContext) Err() error            { panic("typed-nil context touched") }
func (*imageSnapshotPanicContext) Value(any) any         { panic("typed-nil context touched") }

// imageSnapshotOpaque is deliberately non-comparable (func field), so it can
// exercise per-slot closure where duplicate detection is impossible.
type imageSnapshotOpaque struct{ onclose func() }

func (o imageSnapshotOpaque) Read([]byte) (int, error) { return 0, io.EOF }
func (o imageSnapshotOpaque) Close() error {
	o.onclose()
	return nil
}

// imageSnapshotPrepared records how a disposed snapshot was closed.
type imageSnapshotPrepared struct {
	verif      domain.ProbeImageContentVerification
	openCalls  int
	closeCalls int
	closeCtx   context.Context
	closeErr   error
	onVerify   func()
}

func (p *imageSnapshotPrepared) Verification() domain.ProbeImageContentVerification {
	if p.onVerify != nil {
		p.onVerify()
	}
	return p.verif
}

func (p *imageSnapshotPrepared) Open(context.Context) (ports.ProbeImageContentInputs, error) {
	p.openCalls++
	return ports.ProbeImageContentInputs{}, domain.ErrProbeImageSnapshotClosed
}

func (p *imageSnapshotPrepared) Close(ctx context.Context) error {
	p.closeCalls++
	p.closeCtx = ctx
	return p.closeErr
}

// imageSnapshotPanicPrepared is a typed-nil snapshot trap.
type imageSnapshotPanicPrepared struct{}

func (*imageSnapshotPanicPrepared) Verification() domain.ProbeImageContentVerification {
	panic("typed-nil snapshot touched")
}
func (*imageSnapshotPanicPrepared) Open(context.Context) (ports.ProbeImageContentInputs, error) {
	panic("typed-nil snapshot touched")
}
func (*imageSnapshotPanicPrepared) Close(context.Context) error { panic("typed-nil snapshot touched") }

// imageSnapshotPreparer records the delegated request and answers with the
// configured result/error, optionally mutating what it received or canceling
// the caller's context mid-call.
type imageSnapshotPreparer struct {
	calls  int
	pin    domain.ProbeImagePin
	inputs ports.ProbeImageContentInputs
	result ports.PreparedProbeImage
	err    error
	onCall func(ports.ProbeImageContentInputs)
}

func (p *imageSnapshotPreparer) PrepareImageSnapshot(_ context.Context, pin domain.ProbeImagePin, inputs ports.ProbeImageContentInputs) (ports.PreparedProbeImage, error) {
	p.calls++
	p.pin = pin
	p.inputs = inputs
	if p.onCall != nil {
		p.onCall(inputs)
	}
	return p.result, p.err
}

func imageSnapshotPin() domain.ProbeImagePin {
	return domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100}
}

func imageSnapshotVerification(pin domain.ProbeImagePin) domain.ProbeImageContentVerification {
	return domain.ProbeImageContentVerification{
		Selection: domain.ProbeImageSelection{
			Pin:            pin,
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     2,
		},
		LayerBytes:        20,
		UncompressedBytes: 40,
	}
}

func imageSnapshotRequest() (ports.ProbeImageContentInputs, []*imageSnapshotInput) {
	handles := make([]*imageSnapshotInput, 5)
	slots := make([]io.ReadCloser, len(handles))
	for i := range handles {
		handles[i] = &imageSnapshotInput{Reader: bytes.NewReader(nil)}
		slots[i] = handles[i]
	}
	return ports.ProbeImageContentInputs{
		Metadata: ports.ProbeImageInputs{Index: slots[0], Manifest: slots[1], Config: slots[2]},
		Layers:   slots[3:],
	}, handles
}

func assertImageSnapshotDisposed(t *testing.T, p *imageSnapshotPrepared) {
	t.Helper()
	if p.closeCalls != 1 {
		t.Fatal("snapshot not disposed exactly once", p.closeCalls)
	}
	if p.closeCtx == nil {
		t.Fatal("disposal without cleanup context")
	}
	if p.closeCtx.Err() != nil || p.closeCtx.Done() != nil {
		t.Fatal("cleanup context not independent of cancellation", p.closeCtx.Err())
	}
}

// assertImageSnapshotClosedOnce checks the ownership contract on the request
// actually supplied: every distinct supplied typed handle is released exactly
// once, and a comparable duplicate handle is released once in total.
func assertImageSnapshotClosedOnce(t *testing.T, inputs ports.ProbeImageContentInputs) {
	t.Helper()
	seen := map[*imageSnapshotInput]struct{}{}
	slots := append([]io.ReadCloser{inputs.Metadata.Index, inputs.Metadata.Manifest, inputs.Metadata.Config}, inputs.Layers...)
	for _, slot := range slots {
		h, ok := slot.(*imageSnapshotInput)
		if !ok || h == nil {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		if h.closed != 1 {
			t.Fatal("supplied stream not released exactly once", h.closed)
		}
	}
}

func TestProbeImageSnapshotServiceConstructor(t *testing.T) {
	if _, err := NewProbeImageSnapshotService(nil); !errors.Is(err, domain.ErrProbeImageInput) {
		t.Fatal("nil preparer accepted")
	}
	var typedNil *imageSnapshotPreparer
	if _, err := NewProbeImageSnapshotService(typedNil); !errors.Is(err, domain.ErrProbeImageInput) {
		t.Fatal("typed-nil preparer accepted")
	}
	if _, err := NewProbeImageSnapshotService(&imageSnapshotPreparer{}); err != nil {
		t.Fatal("valid preparer refused", err)
	}
}

// Refused requests must reach no adapter I/O and must still release every
// supplied stream exactly once on entry.
func TestProbeImageSnapshotServiceRefusesBeforeIO(t *testing.T) {
	pin := imageSnapshotPin()
	kinds := []string{
		"nil_context", "typed_nil_context", "invalid_pin",
		"nil_index", "nil_manifest", "nil_config", "typed_nil_config",
		"nil_layer", "typed_nil_layer", "zero_layers", "excess_layers",
		"duplicate_layer", "duplicate_cross_slot", "already_canceled",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			adapter := &imageSnapshotPreparer{}
			svc, err := NewProbeImageSnapshotService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			in, _ := imageSnapshotRequest()
			want := domain.ErrProbeImageInput
			request := pin
			callCtx := ctx
			switch kind {
			case "nil_context":
				callCtx = nil
			case "typed_nil_context":
				var nilCtx *imageSnapshotPanicContext
				callCtx = nilCtx
			case "invalid_pin":
				request.Version = "latest"
			case "nil_index":
				in.Metadata.Index = nil
			case "nil_manifest":
				in.Metadata.Manifest = nil
			case "nil_config":
				in.Metadata.Config = nil
			case "typed_nil_config":
				in.Metadata.Config = (*imageSnapshotPanicHandle)(nil)
			case "nil_layer":
				in.Layers[1] = nil
			case "typed_nil_layer":
				in.Layers[0] = (*imageSnapshotPanicHandle)(nil)
			case "zero_layers":
				in.Layers = nil
			case "excess_layers":
				excess := make([]*imageSnapshotInput, domain.MaxProbeImageLayers+1)
				slots := make([]io.ReadCloser, len(excess))
				for i := range excess {
					excess[i] = &imageSnapshotInput{Reader: bytes.NewReader(nil)}
					slots[i] = excess[i]
				}
				in.Layers = slots
			case "duplicate_layer":
				in.Layers[1] = in.Layers[0]
			case "duplicate_cross_slot":
				in.Metadata.Index = in.Layers[0]
			case "already_canceled":
				cancel()
				want = context.Canceled
			}
			got, err := svc.Prepare(callCtx, request, in)
			if got != nil || err == nil {
				t.Fatal("refused request accepted")
			}
			if err != want {
				t.Fatal("error not the bare fixed sentinel", err)
			}
			if adapter.calls != 0 {
				t.Fatal("refused request dispatched")
			}
			assertImageSnapshotClosedOnce(t, in)
		})
	}
}

func TestProbeImageSnapshotServiceDelegation(t *testing.T) {
	pin := imageSnapshotPin()
	t.Run("valid", func(t *testing.T) {
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{result: result}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, handles := imageSnapshotRequest()
		got, err := svc.Prepare(t.Context(), pin, in)
		if err != nil || got != ports.PreparedProbeImage(result) {
			t.Fatal("valid snapshot refused", err)
		}
		if adapter.calls != 1 || adapter.pin != pin {
			t.Fatal("not delegated exactly once with the exact pin")
		}
		if adapter.inputs.Metadata != in.Metadata {
			t.Fatal("metadata handles changed before adapter")
		}
		for i, h := range handles[3:] {
			if adapter.inputs.Layers[i] != h {
				t.Fatal("layer order changed before adapter", i)
			}
		}
		for _, h := range handles {
			if h.closed != 0 {
				t.Fatal("service closed accepted input it delegated", h.closed)
			}
		}
		if result.closeCalls != 0 || result.openCalls != 0 {
			t.Fatal("accepted snapshot touched")
		}
	})
	t.Run("layer_copy", func(t *testing.T) {
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{result: result}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, handles := imageSnapshotRequest()
		if _, err := svc.Prepare(t.Context(), pin, in); err != nil {
			t.Fatal(err)
		}
		received := adapter.inputs.Layers
		other := &imageSnapshotInput{Reader: bytes.NewReader(nil)}
		in.Layers[0] = other
		in.Layers[1] = nil
		in.Layers = in.Layers[:1]
		if len(received) != 2 || received[0] != handles[3] || received[1] != handles[4] {
			t.Fatal("caller layer list mutation escaped the entry copy")
		}
	})
	t.Run("delegate_mutation", func(t *testing.T) {
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{
			result: result,
			onCall: func(in ports.ProbeImageContentInputs) {
				in.Layers[0] = nil
				in.Layers[1] = nil
			},
		}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, handles := imageSnapshotRequest()
		if _, err := svc.Prepare(t.Context(), pin, in); err != nil {
			t.Fatal(err)
		}
		if in.Layers[0] != io.ReadCloser(handles[3]) || in.Layers[1] != io.ReadCloser(handles[4]) {
			t.Fatal("delegate mutation reached the caller layer list")
		}
	})
}

func TestProbeImageSnapshotServiceResults(t *testing.T) {
	pin := imageSnapshotPin()
	cases := []struct {
		kind     string
		want     error // exact bare sentinel; nil means the snapshot is returned
		disposed bool
	}{
		{kind: "valid"},
		{kind: "nil_result", want: domain.ErrProbeImageMetadata},
		{kind: "typed_nil_result", want: domain.ErrProbeImageMetadata},
		{kind: "foreign_pin", want: domain.ErrProbeImageMetadata, disposed: true},
		{kind: "invalid_verification", want: domain.ErrProbeImageMetadata, disposed: true},
		{kind: "partial_error", want: domain.ErrProbeImageUnavailable, disposed: true},
		{kind: "partial_wrapped", want: domain.ErrProbeImageSnapshotBusy, disposed: true},
		{kind: "close_error_ignored", want: domain.ErrProbeImageSnapshotStorage, disposed: true},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
			adapter := &imageSnapshotPreparer{result: result}
			switch tc.kind {
			case "nil_result":
				adapter.result = nil
			case "typed_nil_result":
				adapter.result = (*imageSnapshotPanicPrepared)(nil)
			case "foreign_pin":
				foreign := pin
				foreign.Architecture = "arm64"
				result.verif = imageSnapshotVerification(foreign)
			case "invalid_verification":
				result.verif = domain.ProbeImageContentVerification{}
			case "partial_error":
				adapter.err = errors.New("sensitive-adapter-marker")
			case "partial_wrapped":
				adapter.err = fmt.Errorf("sensitive-adapter-marker: %w", domain.ErrProbeImageSnapshotBusy)
			case "close_error_ignored":
				adapter.err = domain.ErrProbeImageSnapshotStorage
				result.closeErr = errors.New("sensitive-cleanup-marker")
			}
			svc, err := NewProbeImageSnapshotService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			in, _ := imageSnapshotRequest()
			got, err := svc.Prepare(t.Context(), pin, in)
			if tc.want == nil {
				if err != nil || got != ports.PreparedProbeImage(result) {
					t.Fatal("valid snapshot refused", err)
				}
				if result.closeCalls != 0 {
					t.Fatal("accepted snapshot disposed")
				}
				return
			}
			if got != nil || err != tc.want {
				t.Fatal("invalid result not refused with the bare fixed sentinel", err)
			}
			if strings.Contains(err.Error(), "sensitive-") {
				t.Fatal("adapter diagnostics leaked")
			}
			if tc.disposed {
				assertImageSnapshotDisposed(t, result)
			} else if result.closeCalls != 0 {
				t.Fatal("absent snapshot disposed")
			}
		})
	}
}

func TestProbeImageSnapshotServiceErrorAllowlist(t *testing.T) {
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
		{kind: "canceled", err: errors.Join(context.Canceled, errors.New("sensitive-adapter-marker")), want: context.Canceled},
		{kind: "deadline", err: errors.Join(context.DeadlineExceeded, errors.New("sensitive-adapter-marker")), want: context.DeadlineExceeded},
		{kind: "private_error", err: errors.New("sensitive-private-/var/lib/path"), want: domain.ErrProbeImageUnavailable},
		{kind: "wrapped_private", err: fmt.Errorf("sensitive-private-/var/lib/path: %w", errors.New("inner-marker")), want: domain.ErrProbeImageUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			adapter := &imageSnapshotPreparer{err: tc.err}
			svc, err := NewProbeImageSnapshotService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			in, handles := imageSnapshotRequest()
			got, err := svc.Prepare(t.Context(), pin, in)
			if got != nil || err != tc.want {
				t.Fatal("diagnostic not mapped to the bare fixed sentinel", err)
			}
			if strings.Contains(err.Error(), "sensitive-") {
				t.Fatal("adapter diagnostics leaked")
			}
			for _, h := range handles {
				if h.closed != 0 {
					t.Fatal("accepted input closed by the service", h.closed)
				}
			}
		})
	}
}

func TestProbeImageSnapshotServiceCleanupFailure(t *testing.T) {
	for _, kind := range []string{"adapter_error", "canceled_result", "invalid_result", "during_verification", "canceled_storage_error"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			pin := imageSnapshotPin()
			result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin), closeErr: errors.New("private cleanup diagnostic")}
			adapter := &imageSnapshotPreparer{result: result}
			switch kind {
			case "adapter_error":
				adapter.err = errors.New("private adapter diagnostic")
			case "canceled_result":
				adapter.onCall = func(ports.ProbeImageContentInputs) { cancel() }
			case "invalid_result":
				result.verif = domain.ProbeImageContentVerification{}
			case "during_verification":
				result.onVerify = cancel
			case "canceled_storage_error":
				adapter.result = nil
				adapter.err = domain.ErrProbeImageSnapshotStorage
				adapter.onCall = func(ports.ProbeImageContentInputs) { cancel() }
			}
			svc, e := NewProbeImageSnapshotService(adapter)
			if e != nil {
				t.Fatal(e)
			}
			in, _ := imageSnapshotRequest()
			got, e := svc.Prepare(ctx, pin, in)
			if got != nil || e != domain.ErrProbeImageSnapshotStorage {
				t.Fatalf("cleanup failure hidden: %v / %v", got, e)
			}
			if kind != "canceled_storage_error" {
				assertImageSnapshotDisposed(t, result)
			}
		})
	}
}

func TestProbeImageSnapshotServiceCancellation(t *testing.T) {
	pin := imageSnapshotPin()
	t.Run("canceled_during_prepare", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{result: result, onCall: func(ports.ProbeImageContentInputs) { cancel() }}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := imageSnapshotRequest()
		got, err := svc.Prepare(ctx, pin, in)
		if got != nil || err != context.Canceled {
			t.Fatal("cancellation after preparation ignored", err)
		}
		assertImageSnapshotDisposed(t, result)
	})
	t.Run("canceled_during_verification", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin), onVerify: cancel}
		adapter := &imageSnapshotPreparer{result: result}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := imageSnapshotRequest()
		got, err := svc.Prepare(ctx, pin, in)
		if got != nil || err != context.Canceled {
			t.Fatal("cancellation during verification access ignored", err)
		}
		assertImageSnapshotDisposed(t, result)
	})
	t.Run("canceled_cleanup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{
			result: result,
			err:    errors.New("sensitive-adapter-marker"),
			onCall: func(ports.ProbeImageContentInputs) { cancel() },
		}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := imageSnapshotRequest()
		got, err := svc.Prepare(ctx, pin, in)
		if got != nil || err != context.Canceled {
			t.Fatal("cancellation not reported", err)
		}
		if ctx.Err() != context.Canceled {
			t.Fatal("caller context not canceled")
		}
		assertImageSnapshotDisposed(t, result)
	})
}

func TestProbeImageSnapshotServiceHandleOwnership(t *testing.T) {
	pin := imageSnapshotPin()
	t.Run("typed_nil_never_touched", func(t *testing.T) {
		adapter := &imageSnapshotPreparer{}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, _ := imageSnapshotRequest()
		in.Metadata.Config = (*imageSnapshotPanicHandle)(nil)
		in.Layers[1] = (*imageSnapshotPanicHandle)(nil)
		got, err := svc.Prepare(t.Context(), pin, in)
		if got != nil || err != domain.ErrProbeImageInput {
			t.Fatal("typed-nil stream accepted", err)
		}
		if adapter.calls != 0 {
			t.Fatal("refused request dispatched")
		}
		assertImageSnapshotClosedOnce(t, in)
	})
	t.Run("comparable_duplicate_closed_once", func(t *testing.T) {
		adapter := &imageSnapshotPreparer{}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		in, handles := imageSnapshotRequest()
		dup := handles[3]
		in.Layers[0] = dup
		in.Layers[1] = dup
		got, err := svc.Prepare(t.Context(), pin, in)
		if got != nil || err != domain.ErrProbeImageInput {
			t.Fatal("duplicate handles accepted", err)
		}
		if dup.closed != 1 {
			t.Fatal("duplicate handle not closed exactly once", dup.closed)
		}
		assertImageSnapshotClosedOnce(t, in)
	})
	t.Run("non_comparable_closed_per_slot", func(t *testing.T) {
		adapter := &imageSnapshotPreparer{}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		shared, other := 0, 0
		dup := imageSnapshotOpaque{onclose: func() { shared++ }}
		single := imageSnapshotOpaque{onclose: func() { other++ }}
		in, _ := imageSnapshotRequest()
		in.Metadata.Config = single
		in.Layers = []io.ReadCloser{dup, dup}
		request := pin
		request.Version = "latest" // refuse so the service performs the closure
		got, err := svc.Prepare(t.Context(), request, in)
		if got != nil || err != domain.ErrProbeImageInput {
			t.Fatal("refused request accepted", err)
		}
		if shared != 2 {
			t.Fatal("non-comparable handle not closed per supplied slot", shared)
		}
		if other != 1 {
			t.Fatal("non-comparable slot not closed once", other)
		}
		if adapter.calls != 0 {
			t.Fatal("refused request dispatched")
		}
	})
	t.Run("non_comparable_delegated", func(t *testing.T) {
		result := &imageSnapshotPrepared{verif: imageSnapshotVerification(pin)}
		adapter := &imageSnapshotPreparer{result: result}
		svc, err := NewProbeImageSnapshotService(adapter)
		if err != nil {
			t.Fatal(err)
		}
		closed := 0
		layer := imageSnapshotOpaque{onclose: func() { closed++ }}
		in, handles := imageSnapshotRequest()
		in.Layers = []io.ReadCloser{layer, layer}
		got, err := svc.Prepare(t.Context(), pin, in)
		if err != nil || got != ports.PreparedProbeImage(result) {
			t.Fatal("non-comparable handles refused on a valid request", err)
		}
		if adapter.calls != 1 || closed != 0 {
			t.Fatal("accepted non-comparable handles mishandled", adapter.calls, closed)
		}
		for _, h := range handles[:3] {
			if h.closed != 0 {
				t.Fatal("service closed accepted input it delegated", h.closed)
			}
		}
	})
}
