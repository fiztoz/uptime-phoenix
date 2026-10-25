package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type imageContentVerifierFunc func(context.Context, domain.ProbeImagePin, ports.ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error)

func (f imageContentVerifierFunc) VerifyImageContent(ctx context.Context, p domain.ProbeImagePin, in ports.ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error) {
	return f(ctx, p, in)
}

type imageContentInput struct {
	io.Reader
	closed int
}

func (r *imageContentInput) Close() error { r.closed++; return nil }

func TestProbeImageContentServiceContract(t *testing.T) {
	if _, err := NewProbeImageContentService(nil); !errors.Is(err, domain.ErrProbeImageInput) {
		t.Fatal("nil verifier accepted")
	}
	pin := domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100}
	selection := domain.ProbeImageSelection{Pin: pin, ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("c", 64), LayerCount: 2}
	expected := domain.ProbeImageContentVerification{Selection: selection, LayerBytes: 20, UncompressedBytes: 40}
	cases := []struct {
		kind    string
		refused bool  // must be rejected before any verifier dispatch
		want    error // exact bare sentinel; nil means a valid verification
	}{
		{kind: "valid"},
		{kind: "delegate_mutation"},
		{kind: "changed_pin", want: domain.ErrProbeImageMetadata},
		{kind: "invalid_result", want: domain.ErrProbeImageMetadata},
		{kind: "nil_result", want: domain.ErrProbeImageMetadata},
		{kind: "unknown_error", want: domain.ErrProbeImageUnavailable},
		{kind: "wrapped_input", want: domain.ErrProbeImageInput},
		{kind: "wrapped_size", want: domain.ErrProbeImageSize},
		{kind: "wrapped_digest", want: domain.ErrProbeImageDigest},
		{kind: "wrapped_metadata", want: domain.ErrProbeImageMetadata},
		{kind: "wrapped_unavailable", want: domain.ErrProbeImageUnavailable},
		{kind: "wrapped_limit", want: domain.ErrProbeImageContentLimit},
		{kind: "wrapped_encoding", want: domain.ErrProbeImageContentEncoding},
		{kind: "wrapped_diffid", want: domain.ErrProbeImageContentDiffID},
		{kind: "wrapped_canceled", want: context.Canceled},
		{kind: "canceled_result", want: context.Canceled},
		{kind: "canceled_error", want: context.Canceled},
		{kind: "already_canceled", refused: true, want: context.Canceled},
		{kind: "invalid_pin", refused: true, want: domain.ErrProbeImageInput},
		{kind: "nil_context", refused: true, want: domain.ErrProbeImageInput},
		{kind: "nil_index", refused: true, want: domain.ErrProbeImageInput},
		{kind: "nil_manifest", refused: true, want: domain.ErrProbeImageInput},
		{kind: "nil_config", refused: true, want: domain.ErrProbeImageInput},
		{kind: "nil_layer", refused: true, want: domain.ErrProbeImageInput},
		{kind: "zero_layers", refused: true, want: domain.ErrProbeImageInput},
		{kind: "excess_layers", refused: true, want: domain.ErrProbeImageInput},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			calls := 0
			result := expected
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			verifier := imageContentVerifierFunc(func(_ context.Context, p domain.ProbeImagePin, in ports.ProbeImageContentInputs) (*domain.ProbeImageContentVerification, error) {
				calls++
				if p != pin {
					t.Fatal("pin changed before adapter")
				}
				// Ownership transferred: the delegate closes each received
				// stream exactly once and may not be closed again by the service.
				received := append([]io.ReadCloser(nil), in.Layers...)
				for _, r := range append(received, in.Metadata.Index, in.Metadata.Manifest, in.Metadata.Config) {
					_ = r.Close()
				}
				switch tc.kind {
				case "delegate_mutation":
					for i := range in.Layers {
						in.Layers[i] = nil
					}
				case "changed_pin":
					result.Selection.Pin.Version = "1.2.4"
				case "invalid_result":
					result.Selection.LayerCount = 0
				case "nil_result":
					return nil, nil
				case "unknown_error":
					return nil, errors.New("sensitive-adapter-marker")
				case "wrapped_input":
					return nil, errors.Join(domain.ErrProbeImageInput, errors.New("sensitive-adapter-marker"))
				case "wrapped_size":
					return nil, errors.Join(domain.ErrProbeImageSize, errors.New("sensitive-adapter-marker"))
				case "wrapped_digest":
					return nil, errors.Join(domain.ErrProbeImageDigest, errors.New("sensitive-adapter-marker"))
				case "wrapped_metadata":
					return nil, errors.Join(domain.ErrProbeImageMetadata, errors.New("sensitive-adapter-marker"))
				case "wrapped_unavailable":
					return nil, errors.Join(domain.ErrProbeImageUnavailable, errors.New("sensitive-adapter-marker"))
				case "wrapped_limit":
					return nil, errors.Join(domain.ErrProbeImageContentLimit, errors.New("sensitive-adapter-marker"))
				case "wrapped_encoding":
					return nil, errors.Join(domain.ErrProbeImageContentEncoding, errors.New("sensitive-adapter-marker"))
				case "wrapped_diffid":
					return nil, errors.Join(domain.ErrProbeImageContentDiffID, errors.New("sensitive-adapter-marker"))
				case "wrapped_canceled":
					return nil, errors.Join(context.Canceled, errors.New("sensitive-adapter-marker"))
				case "canceled_result":
					cancel()
				case "canceled_error":
					cancel()
					return nil, errors.New("sensitive-adapter-marker")
				}
				return &result, nil
			})
			svc, err := NewProbeImageContentService(verifier)
			if err != nil {
				t.Fatal(err)
			}
			meta := []*imageContentInput{{Reader: bytes.NewReader(nil)}, {Reader: bytes.NewReader(nil)}, {Reader: bytes.NewReader(nil)}}
			layerMocks := []*imageContentInput{{Reader: bytes.NewReader(nil)}, {Reader: bytes.NewReader(nil)}}
			in := ports.ProbeImageContentInputs{
				Metadata: ports.ProbeImageInputs{Index: meta[0], Manifest: meta[1], Config: meta[2]},
				Layers:   []io.ReadCloser{layerMocks[0], layerMocks[1]},
			}
			request := pin
			callCtx := ctx
			switch tc.kind {
			case "invalid_pin":
				request.Version = "latest"
			case "nil_context":
				callCtx = nil
			case "nil_index":
				in.Metadata.Index = nil
			case "nil_manifest":
				in.Metadata.Manifest = nil
			case "nil_config":
				in.Metadata.Config = nil
			case "nil_layer":
				in.Layers[1] = nil
			case "zero_layers":
				in.Layers = nil
			case "excess_layers":
				excess := make([]*imageContentInput, domain.MaxProbeImageLayers+1)
				excessStreams := make([]io.ReadCloser, len(excess))
				for i := range excess {
					excess[i] = &imageContentInput{Reader: bytes.NewReader(nil)}
					excessStreams[i] = excess[i]
				}
				layerMocks = excess
				in.Layers = excessStreams
			case "already_canceled":
				cancel()
			}
			// Snapshot exactly the streams whose ownership transfers on entry.
			var owned []*imageContentInput
			capture := func(r io.ReadCloser) {
				if c, ok := r.(*imageContentInput); ok && c != nil {
					owned = append(owned, c)
				}
			}
			capture(in.Metadata.Index)
			capture(in.Metadata.Manifest)
			capture(in.Metadata.Config)
			for _, r := range in.Layers {
				capture(r)
			}
			got, err := svc.Verify(callCtx, request, in)
			if tc.want == nil {
				if err != nil || got == nil || *got != expected {
					t.Fatal("valid verification lost", err)
				}
				result.UncompressedBytes++
				if got.UncompressedBytes != expected.UncompressedBytes {
					t.Fatal("borrowed adapter result escaped")
				}
			} else {
				if got != nil || err == nil {
					t.Fatal("invalid result/input accepted")
				}
				if err != tc.want {
					t.Fatal("error not the bare fixed sentinel", err)
				}
				if strings.Contains(err.Error(), "sensitive-") {
					t.Fatal("adapter diagnostics leaked")
				}
			}
			if tc.refused {
				if calls != 0 {
					t.Fatal("refused request dispatched")
				}
			} else if calls != 1 {
				t.Fatal("verifier not delegated exactly once")
			}
			for _, r := range owned {
				if r.closed != 1 {
					t.Fatal("owned input not released exactly once")
				}
			}
			if tc.kind == "delegate_mutation" {
				if in.Layers[0] != io.ReadCloser(layerMocks[0]) || in.Layers[1] != io.ReadCloser(layerMocks[1]) {
					t.Fatal("caller layer list mutated")
				}
			}
		})
	}
}
