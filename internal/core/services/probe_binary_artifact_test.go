package services

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type binaryTestInput struct {
	io.Reader
	closed bool
}

func (i *binaryTestInput) Close() error { i.closed = true; return nil }

type binaryTestPrepared struct {
	pin    domain.ProbeBinaryPin
	closed bool
}

func (p *binaryTestPrepared) Pin() domain.ProbeBinaryPin { return p.pin }
func (p *binaryTestPrepared) Open(context.Context) (io.ReadCloser, error) {
	return nil, domain.ErrProbeArtifactClosed
}
func (p *binaryTestPrepared) Close(context.Context) error { p.closed = true; return nil }

type binaryTestPreparer struct {
	calls  int
	result ports.PreparedProbeBinary
	err    error
	cancel context.CancelFunc
}

func (p *binaryTestPreparer) PrepareBinary(_ context.Context, _ domain.ProbeBinaryPin, input io.ReadCloser) (ports.PreparedProbeBinary, error) {
	p.calls++
	_ = input.Close()
	if p.cancel != nil {
		p.cancel()
	}
	return p.result, p.err
}

func TestProbeBinaryPreparationService(t *testing.T) {
	pin := domain.ProbeBinaryPin{Version: "0.4.5", Architecture: "amd64", Size: 1, SHA256: strings.Repeat("a", 64)}
	for _, kind := range []string{"valid", "invalid", "canceled", "mismatch", "nil_result", "partial_error", "canceled_after_prepare"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := &binaryTestPrepared{pin: pin}
			adapter := &binaryTestPreparer{result: result}
			svc, err := NewProbeBinaryPreparationService(adapter)
			if err != nil {
				t.Fatal(err)
			}
			input := &binaryTestInput{Reader: strings.NewReader("a")}
			want := pin
			switch kind {
			case "invalid":
				want.Size = 0
			case "canceled":
				cancel()
			case "mismatch":
				result.pin.Architecture = "arm64"
			case "nil_result":
				adapter.result = nil
			case "partial_error":
				adapter.err = io.ErrUnexpectedEOF
			case "canceled_after_prepare":
				adapter.cancel = cancel
			}
			prepared, err := svc.Prepare(ctx, want, input)
			if kind == "valid" {
				if err != nil || prepared != result {
					t.Fatal("valid result refused", err)
				}
			} else if err == nil || prepared != nil {
				t.Fatal("invalid result escaped")
			}
			if !input.closed {
				t.Fatal("input ownership leaked")
			}
			if (kind == "invalid" || kind == "canceled") && adapter.calls != 0 {
				t.Fatal("I/O before validation")
			}
			if (kind == "mismatch" || kind == "partial_error" || kind == "canceled_after_prepare") && !result.closed {
				t.Fatal("partial snapshot leaked")
			}
			if kind == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	if _, err := NewProbeBinaryPreparationService(nil); err == nil {
		t.Fatal("nil port")
	}
}
