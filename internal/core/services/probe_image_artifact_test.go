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

type imagePreparerFunc func(context.Context, domain.ProbeImagePin, ports.ProbeImageInputs) (*domain.ProbeImageSelection, error)

func (f imagePreparerFunc) PrepareImage(ctx context.Context, p domain.ProbeImagePin, in ports.ProbeImageInputs) (*domain.ProbeImageSelection, error) {
	return f(ctx, p, in)
}

type imageServiceInput struct {
	io.Reader
	closed int
}

func (r *imageServiceInput) Close() error { r.closed++; return nil }

func TestProbeImagePreparationServiceContract(t *testing.T) {
	if _, err := NewProbeImagePreparationService(nil); !errors.Is(err, domain.ErrProbeImageInput) {
		t.Fatal("nil preparer accepted")
	}
	pin := domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100}
	valid := domain.ProbeImageSelection{Pin: pin, ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("c", 64), LayerCount: 1}
	for _, kind := range []string{"valid", "changed_pin", "bad_manifest", "bad_config", "bad_layers", "nil_result", "unknown_error", "wrapped_error", "canceled_result", "invalid_pin", "nil_context", "nil_stream", "already_canceled"} {
		t.Run(kind, func(t *testing.T) {
			called := false
			result := valid
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			preparer := imagePreparerFunc(func(_ context.Context, p domain.ProbeImagePin, in ports.ProbeImageInputs) (*domain.ProbeImageSelection, error) {
				called = true
				if p != pin {
					t.Fatal("pin changed before adapter")
				}
				for _, r := range []io.ReadCloser{in.Index, in.Manifest, in.Config} {
					_ = r.Close()
				}
				switch kind {
				case "changed_pin":
					result.Pin.Version = "1.2.4"
				case "bad_manifest":
					result.ManifestSHA256 = "bad"
				case "bad_config":
					result.ConfigSHA256 = "bad"
				case "bad_layers":
					result.LayerCount = 0
				case "nil_result":
					return nil, nil
				case "unknown_error":
					return nil, errors.New("sensitive-adapter-marker")
				case "wrapped_error":
					return nil, errors.Join(domain.ErrProbeImageMetadata, errors.New("sensitive-adapter-marker"))
				case "canceled_result":
					cancel()
				}
				return &result, nil
			})
			svc, err := NewProbeImagePreparationService(preparer)
			if err != nil {
				t.Fatal(err)
			}
			all := []*imageServiceInput{{Reader: bytes.NewReader(nil)}, {Reader: bytes.NewReader(nil)}, {Reader: bytes.NewReader(nil)}}
			in := ports.ProbeImageInputs{Index: all[0], Manifest: all[1], Config: all[2]}
			request := pin
			var callCtx context.Context = ctx
			switch kind {
			case "invalid_pin":
				request.Version = "latest"
			case "nil_context":
				callCtx = nil
			case "nil_stream":
				in.Config = nil
				all = all[:2]
			case "already_canceled":
				cancel()
			}
			got, err := svc.Prepare(callCtx, request, in)
			if kind == "valid" {
				if err != nil || got == nil || *got != valid {
					t.Fatal("valid selection lost", err)
				}
				result.ConfigSHA256 = strings.Repeat("d", 64)
				if got.ConfigSHA256 != valid.ConfigSHA256 {
					t.Fatal("borrowed adapter result escaped")
				}
			} else {
				if got != nil || err == nil {
					t.Fatal("invalid result/input accepted")
				}
				if strings.Contains(err.Error(), "sensitive-") {
					t.Fatal("adapter diagnostics leaked")
				}
				if kind == "unknown_error" && err != domain.ErrProbeImageUnavailable {
					t.Fatal("unknown adapter failure not sanitized")
				}
				if kind == "wrapped_error" && err != domain.ErrProbeImageMetadata {
					t.Fatal("typed adapter failure not sanitized")
				}
				if (kind == "canceled_result" || kind == "already_canceled") && !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation not dominant")
				}
			}
			if (kind == "invalid_pin" || kind == "nil_context" || kind == "nil_stream" || kind == "already_canceled") && called {
				t.Fatal("invalid request dispatched")
			}
			for _, r := range all {
				if r.closed != 1 {
					t.Fatal("owned input not released exactly once")
				}
			}
		})
	}
}
