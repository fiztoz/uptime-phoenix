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

// receivePanicPreparer panics if a typed-nil port is dispatched.
type receivePanicPreparer struct{}

func (*receivePanicPreparer) PrepareBinary(context.Context, domain.ProbeBinaryPin, io.ReadCloser) (ports.PreparedProbeBinary, error) {
	panic("typed-nil preparer touched")
}

type receivePreparer struct {
	calls      int
	pin        domain.ProbeBinaryPin
	input      io.ReadCloser
	result     ports.PreparedProbeBinary
	err        error
	consume    bool
	closeChild bool
	readErr    error
	onCall     func()
}

func (p *receivePreparer) PrepareBinary(ctx context.Context, pin domain.ProbeBinaryPin, input io.ReadCloser) (ports.PreparedProbeBinary, error) {
	p.calls++
	p.pin = pin
	p.input = input
	if p.onCall != nil {
		p.onCall()
	}
	if p.consume && input != nil {
		_, p.readErr = io.Copy(io.Discard, input)
	}
	if p.closeChild && input != nil {
		_ = input.Close()
	}
	if ctx.Err() != nil && p.err == nil && p.result == nil {
		return nil, ctx.Err()
	}
	if p.err != nil {
		return p.result, p.err
	}
	if p.readErr != nil {
		return p.result, p.readErr
	}
	return p.result, nil
}

type receiveBinary struct {
	pin      domain.ProbeBinaryPin
	opens    int
	closes   int
	closeErr error
	closeCtx context.Context
	onClose  func()
}

func (b *receiveBinary) Pin() domain.ProbeBinaryPin { return b.pin }

func (b *receiveBinary) Open(context.Context) (io.ReadCloser, error) {
	b.opens++
	return nil, errors.New(stagePrivateMarker)
}

func (b *receiveBinary) Close(ctx context.Context) error {
	b.closes++
	b.closeCtx = ctx
	if b.onClose != nil {
		b.onClose()
	}
	return b.closeErr
}

type receiveWriter struct {
	calls        int
	ctx          context.Context
	intent       domain.ProbeBinaryStageIntent
	binary       ports.PreparedProbeBinary
	seal         domain.ProbeBinaryStageSeal
	err          error
	atCall       func()
	inputClosed  bool
	binaryClosed bool
	binaryOpened bool
}

func (m *receiveWriter) StageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	m.calls++
	m.ctx = ctx
	m.intent = intent
	m.binary = binary
	if m.atCall != nil {
		m.atCall()
	}
	return m.seal, m.err
}

type receiveStream struct {
	r        io.Reader
	closes   int
	closeErr error
	onClose  func()
}

func (s *receiveStream) Read(p []byte) (int, error) {
	if s.r == nil {
		return 0, io.EOF
	}
	return s.r.Read(p)
}

func (s *receiveStream) Close() error {
	s.closes++
	if s.onClose != nil {
		s.onClose()
	}
	if s.closeErr != nil {
		return s.closeErr
	}
	return nil
}

type receiveScript struct {
	data  []byte
	empty int
	after func() (int, error)
}

func (s *receiveScript) Read(p []byte) (int, error) {
	if len(s.data) > 0 {
		n := copy(p, s.data)
		s.data = s.data[n:]
		return n, nil
	}
	s.empty++
	if s.after != nil {
		return s.after()
	}
	return 0, nil
}

type receiveContext struct {
	context.Context
	err error
}

func (c receiveContext) Err() error {
	if c.err != nil {
		return c.err
	}
	if c.Context == nil {
		return nil
	}
	return c.Context.Err()
}

func newReceiveService(t *testing.T, preparer ports.ProbeBinaryPreparer, writer ports.ProbeBinaryStageWriter) *ProbeBinaryStageReceiveService {
	t.Helper()
	svc, err := NewProbeBinaryStageReceiveService(preparer, writer)
	if err != nil || svc == nil {
		t.Fatalf("constructor: %v", err)
	}
	return svc
}

func receiveBytes(n int) *receiveStream {
	return &receiveStream{r: bytes.NewReader(bytes.Repeat([]byte{0x7f}, n))}
}

func receiveExactBinary(intent domain.ProbeBinaryStageIntent) *receiveBinary {
	return &receiveBinary{pin: intent.Identity.Binary}
}

func assertReceiveZero(t *testing.T, got domain.ProbeBinaryStageSeal, err, want error) {
	t.Helper()
	if err != want || got != (domain.ProbeBinaryStageSeal{}) {
		t.Fatalf("got (%+v, %v), want zero and %v", got, err, want)
	}
	if err != nil && (err.Error() != want.Error() || strings.Contains(err.Error(), stagePrivateMarker)) {
		t.Fatalf("diagnostic leaked or is not the bare sentinel: %q", err.Error())
	}
}

func TestProbeBinaryStageReceiveServiceConstructor(t *testing.T) {
	writer := &receiveWriter{}
	if svc, err := NewProbeBinaryStageReceiveService(nil, writer); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("nil preparer accepted", svc, err)
	}
	var typedPreparer *receivePanicPreparer
	if svc, err := NewProbeBinaryStageReceiveService(typedPreparer, writer); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("typed-nil preparer accepted", svc, err)
	}
	var typedIface ports.ProbeBinaryPreparer = (*receivePanicPreparer)(nil)
	if svc, err := NewProbeBinaryStageReceiveService(typedIface, &receiveWriter{}); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("typed-nil preparer interface accepted", svc, err)
	}
	if svc, err := NewProbeBinaryStageReceiveService(&receivePreparer{}, nil); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("nil writer accepted", svc, err)
	}
	var typedWriter *stageWritePanicWriter
	if svc, err := NewProbeBinaryStageReceiveService(&receivePreparer{}, typedWriter); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("typed-nil writer accepted", svc, err)
	}
	svc := newReceiveService(t, &receivePreparer{}, &receiveWriter{})
	if svc.stage == nil || probeBinaryStageNil(svc.preparer) || probeBinaryStageNil(svc.stage.writer) {
		t.Fatal("constructor did not retain both ports")
	}
}

func TestProbeBinaryStageReceiveServiceRefusesBeforeIO(t *testing.T) {
	intent := stageIntent()
	preparer := &receivePreparer{}
	writer := &receiveWriter{}
	svc := newReceiveService(t, preparer, writer)

	t.Run("nil_service", func(t *testing.T) {
		stream := receiveBytes(1)
		var missing *ProbeBinaryStageReceiveService
		got, err := missing.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || preparer.calls != 0 || writer.calls != 0 {
			t.Fatalf("input closes=%d preparer=%d writer=%d", stream.closes, preparer.calls, writer.calls)
		}
	})

	t.Run("nil_input", func(t *testing.T) {
		got, err := svc.Receive(context.Background(), intent, nil)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if preparer.calls != 0 || writer.calls != 0 {
			t.Fatal("ports called for nil input")
		}
	})

	t.Run("typed_nil_input", func(t *testing.T) {
		var stream *receiveStream
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if preparer.calls != 0 || writer.calls != 0 {
			t.Fatal("typed-nil input dispatched")
		}
	})

	t.Run("typed_nil_context", func(t *testing.T) {
		stream := receiveBytes(1)
		var ctx *stageWritePrivateContext
		got, err := svc.Receive(ctx, intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || preparer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d", stream.closes, preparer.calls)
		}
	})

	t.Run("typed_nil_preparer", func(t *testing.T) {
		stream := receiveBytes(1)
		stage, err := NewProbeBinaryStageWriteService(writer)
		if err != nil {
			t.Fatal(err)
		}
		broken := &ProbeBinaryStageReceiveService{preparer: (*receivePanicPreparer)(nil), stage: stage}
		got, recvErr := broken.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, recvErr, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || writer.calls != 0 {
			t.Fatalf("closes=%d writer=%d", stream.closes, writer.calls)
		}
	})

	t.Run("nil_stage", func(t *testing.T) {
		stream := receiveBytes(1)
		broken := &ProbeBinaryStageReceiveService{preparer: preparer}
		got, err := broken.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || preparer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d", stream.closes, preparer.calls)
		}
	})

	t.Run("invalid_intent", func(t *testing.T) {
		stream := receiveBytes(1)
		got, err := svc.Receive(context.Background(), stageDockerIntent(), stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || preparer.calls != 0 || writer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d writer=%d", stream.closes, preparer.calls, writer.calls)
		}
	})

	t.Run("canceled", func(t *testing.T) {
		stream := receiveBytes(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := svc.Receive(ctx, intent, stream)
		assertReceiveZero(t, got, err, context.Canceled)
		if stream.closes != 1 || preparer.calls != 0 || writer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d writer=%d", stream.closes, preparer.calls, writer.calls)
		}
	})

	t.Run("deadline", func(t *testing.T) {
		stream := receiveBytes(1)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		got, err := svc.Receive(ctx, intent, stream)
		assertReceiveZero(t, got, err, context.DeadlineExceeded)
		if stream.closes != 1 || preparer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d", stream.closes, preparer.calls)
		}
	})

	t.Run("wrapped_cancel", func(t *testing.T) {
		stream := receiveBytes(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := svc.Receive(receiveContext{Context: ctx, err: fmt.Errorf("%s: %w", stagePrivateMarker, context.Canceled)}, intent, stream)
		assertReceiveZero(t, got, err, context.Canceled)
		if stream.closes != 1 || preparer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d", stream.closes, preparer.calls)
		}
	})

	t.Run("private_context", func(t *testing.T) {
		stream := receiveBytes(1)
		got, err := svc.Receive(receiveContext{Context: context.Background(), err: errors.New(stagePrivateMarker)}, intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteInput)
		if stream.closes != 1 || preparer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d", stream.closes, preparer.calls)
		}
	})

	t.Run("close_outranks_cancel", func(t *testing.T) {
		stream := receiveBytes(1)
		stream.closeErr = errors.New(stagePrivateMarker)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := svc.Receive(ctx, domain.ProbeBinaryStageIntent{}, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if stream.closes != 1 || preparer.calls != 0 || writer.calls != 0 {
			t.Fatalf("closes=%d preparer=%d writer=%d", stream.closes, preparer.calls, writer.calls)
		}
	})
}

func TestProbeBinaryStageReceiveServicePreparationFailures(t *testing.T) {
	intent := stageIntent()
	cases := []struct {
		name   string
		err    error
		want   error
		binary bool
	}{
		{name: "wrapped_input", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeArtifactInput), want: domain.ErrProbeArtifactInput, binary: true},
		{name: "wrapped_size", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeArtifactSize), want: domain.ErrProbeArtifactSize},
		{name: "wrapped_digest", err: errors.Join(domain.ErrProbeArtifactDigest, errors.New(stagePrivateMarker)), want: domain.ErrProbeArtifactDigest, binary: true},
		{name: "wrapped_metadata", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeArtifactMetadata), want: domain.ErrProbeArtifactMetadata},
		{name: "wrapped_closed", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeArtifactClosed), want: domain.ErrProbeArtifactClosed, binary: true},
		{name: "wrapped_storage", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeArtifactStorage), want: domain.ErrProbeArtifactStorage},
		{name: "wrapped_stage_input", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteInput), want: domain.ErrProbeBinaryStageWriteInput},
		{name: "wrapped_conflict", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteConflict), want: domain.ErrProbeBinaryStageWriteConflict},
		{name: "wrapped_unavailable", err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteUnavailable), want: domain.ErrProbeBinaryStageWriteUnavailable},
		{name: "wrapped_unknown", err: errors.Join(domain.ErrProbeArtifactDigest, domain.ErrProbeBinaryStageWriteUnknown), want: domain.ErrProbeBinaryStageWriteUnknown, binary: true},
		{name: "wrapped_canceled", err: fmt.Errorf("%s: %w", stagePrivateMarker, context.Canceled), want: context.Canceled},
		{name: "wrapped_deadline", err: fmt.Errorf("%s: %w", stagePrivateMarker, context.DeadlineExceeded), want: context.DeadlineExceeded},
		{name: "private", err: errors.New(stagePrivateMarker), want: domain.ErrProbeArtifactStorage, binary: true},
		{name: "storage_outranks_digest", err: errors.Join(domain.ErrProbeArtifactDigest, domain.ErrProbeArtifactStorage), want: domain.ErrProbeArtifactStorage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := receiveExactBinary(intent)
			var result ports.PreparedProbeBinary
			if tc.binary {
				result = bin
			}
			preparer := &receivePreparer{result: result, err: tc.err}
			writer := &receiveWriter{}
			svc := newReceiveService(t, preparer, writer)
			stream := receiveBytes(int(intent.Identity.Binary.Size))
			got, err := svc.Receive(context.Background(), intent, stream)
			assertReceiveZero(t, got, err, tc.want)
			if preparer.calls != 1 || writer.calls != 0 || bin.opens != 0 || stream.closes != 1 {
				t.Fatalf("preparer=%d writer=%d opens=%d closes=%d", preparer.calls, writer.calls, bin.opens, stream.closes)
			}
			if preparer.pin != intent.Identity.Binary {
				t.Fatal("preparer pin was not the captured intent pin")
			}
			wantCloses := 0
			if tc.binary {
				wantCloses = 1
			}
			if bin.closes != wantCloses {
				t.Fatalf("snapshot closes=%d want %d", bin.closes, wantCloses)
			}
		})
	}

	t.Run("nil_result", func(t *testing.T) {
		preparer := &receivePreparer{consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size))
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactMetadata)
		if writer.calls != 0 || stream.closes != 1 {
			t.Fatalf("writer=%d closes=%d", writer.calls, stream.closes)
		}
	})

	t.Run("typed_nil_result", func(t *testing.T) {
		preparer := &receivePreparer{result: (*receiveBinary)(nil), consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size))
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactMetadata)
		if writer.calls != 0 || stream.closes != 1 {
			t.Fatalf("writer=%d closes=%d", writer.calls, stream.closes)
		}
	})

	t.Run("foreign_pin", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		bin.pin.SHA256 = strings.Repeat("cd", 32)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size) + 1)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactMetadata)
		if writer.calls != 0 || bin.closes != 1 || bin.opens != 0 || stream.closes != 1 {
			t.Fatalf("writer=%d closes=%d opens=%d input=%d", writer.calls, bin.closes, bin.opens, stream.closes)
		}
	})

	t.Run("invalid_pin", func(t *testing.T) {
		bin := &receiveBinary{}
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactMetadata)
		if writer.calls != 0 || bin.closes != 1 {
			t.Fatalf("writer=%d closes=%d", writer.calls, bin.closes)
		}
	})

	t.Run("short_payload", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size) - 1)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactSize)
		if writer.calls != 0 || bin.closes != 1 || bin.opens != 0 || stream.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d opens=%d input=%d", writer.calls, bin.closes, bin.opens, stream.closes)
		}
	})

	t.Run("empty_payload", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(0))
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactSize)
		if writer.calls != 0 || bin.closes != 1 {
			t.Fatalf("writer=%d closes=%d", writer.calls, bin.closes)
		}
	})

	t.Run("preparer_does_not_consume", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactSize)
		if writer.calls != 0 || bin.closes != 1 {
			t.Fatalf("writer=%d closes=%d", writer.calls, bin.closes)
		}
	})

	t.Run("close_outranks_digest", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, err: domain.ErrProbeArtifactDigest}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(1)
		stream.closeErr = errors.New(stagePrivateMarker)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if writer.calls != 0 || bin.closes != 1 || stream.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d input=%d", writer.calls, bin.closes, stream.closes)
		}
	})

	t.Run("snapshot_close_outranks_cancel", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		bin.closeErr = errors.New(stagePrivateMarker)
		ctx, cancel := context.WithCancel(context.Background())
		preparer := &receivePreparer{result: bin, consume: true, onCall: cancel}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size))
		got, err := svc.Receive(ctx, intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if writer.calls != 0 || bin.closes != 1 || bin.closeCtx == nil || bin.closeCtx.Err() != nil {
			t.Fatalf("writer=%d closes=%d cleanup=%v", writer.calls, bin.closes, bin.closeCtx)
		}
	})
}

func TestProbeBinaryStageReceiveServiceEOFBeforeStage(t *testing.T) {
	intent := stageIntent()

	t.Run("extra_byte", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size) + 1)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactSize)
		if preparer.calls != 1 || writer.calls != 0 || bin.closes != 1 || bin.opens != 0 || stream.closes != 1 {
			t.Fatalf("preparer=%d writer=%d snapshot=%d opens=%d input=%d", preparer.calls, writer.calls, bin.closes, bin.opens, stream.closes)
		}
		if preparer.input == stream {
			t.Fatal("preparer received the outer input instead of the bounded child")
		}
	})

	t.Run("no_progress", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		script := &receiveScript{data: bytes.Repeat([]byte{0x7f}, int(intent.Identity.Binary.Size))}
		stream := &receiveStream{r: script}
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if writer.calls != 0 || bin.closes != 1 || bin.opens != 0 || stream.closes != 1 || script.empty < 100 {
			t.Fatalf("writer=%d snapshot=%d input=%d empty=%d", writer.calls, bin.closes, stream.closes, script.empty)
		}
	})

	t.Run("confirm_read_failure", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		script := &receiveScript{
			data: bytes.Repeat([]byte{0x7f}, int(intent.Identity.Binary.Size)),
			after: func() (int, error) {
				return 0, errors.New(stagePrivateMarker)
			},
		}
		stream := &receiveStream{r: script}
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if writer.calls != 0 || bin.closes != 1 || stream.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d input=%d", writer.calls, bin.closes, stream.closes)
		}
	})

	t.Run("finish_close_outranks_cancel", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		ctx, cancel := context.WithCancel(context.Background())
		stream := receiveBytes(int(intent.Identity.Binary.Size))
		stream.closeErr = errors.New(stagePrivateMarker)
		stream.onClose = cancel
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(ctx, intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeArtifactStorage)
		if writer.calls != 0 || bin.closes != 1 || stream.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d input=%d", writer.calls, bin.closes, stream.closes)
		}
		if bin.closeCtx == nil || bin.closeCtx.Err() != nil {
			t.Fatal("pre-stage cleanup followed caller cancellation", bin.closeCtx)
		}
	})
}

func TestProbeBinaryStageReceiveServiceStageOutcomes(t *testing.T) {
	intent := stageIntent()
	success := domain.ProbeBinaryStageSeal{Intent: intent}

	t.Run("live_conflict", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{err: fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteConflict)}
		svc := newReceiveService(t, preparer, writer)
		stream := receiveBytes(int(intent.Identity.Binary.Size))
		got, err := svc.Receive(context.Background(), intent, stream)
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteConflict)
		if writer.calls != 1 || bin.closes != 1 || bin.opens != 0 || stream.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d opens=%d input=%d", writer.calls, bin.closes, bin.opens, stream.closes)
		}
	})

	t.Run("live_allowlist", func(t *testing.T) {
		for _, want := range []error{
			domain.ErrProbeBinaryStageWriteInput,
			domain.ErrProbeBinaryStageWriteUnavailable,
			domain.ErrProbeBinaryStageWriteUnknown,
			context.Canceled,
			context.DeadlineExceeded,
		} {
			bin := receiveExactBinary(intent)
			preparer := &receivePreparer{result: bin, consume: true}
			writer := &receiveWriter{err: fmt.Errorf("%s: %w", stagePrivateMarker, want)}
			svc := newReceiveService(t, preparer, writer)
			got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
			assertReceiveZero(t, got, err, want)
			if writer.calls != 1 || bin.closes != 1 || bin.opens != 0 {
				t.Fatalf("%v writer=%d snapshot=%d opens=%d", want, writer.calls, bin.closes, bin.opens)
			}
		}
	})

	t.Run("unknown_outranks_joined_conflict", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{err: errors.Join(domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteUnknown, errors.New(stagePrivateMarker))}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d", writer.calls, bin.closes)
		}
	})

	t.Run("private_stage_error", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{err: errors.New(stagePrivateMarker)}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d", writer.calls, bin.closes)
		}
	})

	t.Run("error_bearing_seal", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{seal: success, err: domain.ErrProbeBinaryStageWriteConflict}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d", writer.calls, bin.closes)
		}
	})

	t.Run("foreign_seal", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		other := success
		other.Intent.AttemptID = "44444444-4444-4444-8444-444444444444"
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{seal: other}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d", writer.calls, bin.closes)
		}
	})

	t.Run("cleanup_revokes_conflict", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		bin.closeErr = errors.New(stagePrivateMarker)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{err: domain.ErrProbeBinaryStageWriteConflict}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 {
			t.Fatalf("writer=%d snapshot=%d", writer.calls, bin.closes)
		}
	})

	t.Run("cleanup_revokes_success", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		bin.closeErr = errors.New(stagePrivateMarker)
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{seal: success}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(context.Background(), intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 || bin.opens != 0 {
			t.Fatalf("writer=%d snapshot=%d opens=%d", writer.calls, bin.closes, bin.opens)
		}
	})

	t.Run("cancel_during_stage", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		ctx, cancel := context.WithCancel(context.Background())
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{seal: success, atCall: cancel}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(ctx, intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 || bin.closeCtx == nil || bin.closeCtx.Err() != nil {
			t.Fatalf("writer=%d snapshot=%d cleanup=%v", writer.calls, bin.closes, bin.closeCtx)
		}
	})

	t.Run("cancel_after_stage", func(t *testing.T) {
		bin := receiveExactBinary(intent)
		ctx, cancel := context.WithCancel(context.Background())
		bin.onClose = cancel
		preparer := &receivePreparer{result: bin, consume: true}
		writer := &receiveWriter{seal: success}
		svc := newReceiveService(t, preparer, writer)
		got, err := svc.Receive(ctx, intent, receiveBytes(int(intent.Identity.Binary.Size)))
		assertReceiveZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || bin.closes != 1 || bin.opens != 0 {
			t.Fatalf("writer=%d snapshot=%d opens=%d", writer.calls, bin.closes, bin.opens)
		}
	})
}

func TestProbeBinaryStageReceiveServiceStagesExactStream(t *testing.T) {
	intent := stageIntent()
	bin := receiveExactBinary(intent)
	stream := receiveBytes(int(intent.Identity.Binary.Size))
	preparer := &receivePreparer{result: bin, consume: true, closeChild: true}
	writer := &receiveWriter{seal: domain.ProbeBinaryStageSeal{Intent: intent}}
	writer.atCall = func() {
		writer.inputClosed = stream.closes > 0
		writer.binaryClosed = bin.closes > 0
		writer.binaryOpened = bin.opens > 0
	}
	svc := newReceiveService(t, preparer, writer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got, err := svc.Receive(ctx, intent, stream)
	if err != nil || got != writer.seal || got.Intent != intent {
		t.Fatalf("got (%+v, %v), want exact seal", got, err)
	}
	if preparer.calls != 1 || preparer.pin != intent.Identity.Binary || preparer.input == stream {
		t.Fatalf("preparer calls=%d same-input=%v", preparer.calls, preparer.input == stream)
	}
	if writer.calls != 1 || writer.intent != intent || writer.binary != bin || writer.ctx != ctx {
		t.Fatal("stage call was not the captured intent and borrowed snapshot")
	}
	if !writer.inputClosed || writer.binaryClosed || writer.binaryOpened {
		t.Fatal("stage ran before input finish or after snapshot mutation", writer)
	}
	if stream.closes != 1 || bin.closes != 1 || bin.opens != 0 {
		t.Fatalf("input=%d snapshot=%d opens=%d", stream.closes, bin.closes, bin.opens)
	}
	if bin.closeCtx == nil || bin.closeCtx.Err() != nil {
		t.Fatal("snapshot cleanup was bound to a canceled context")
	}

	secondBin := receiveExactBinary(intent)
	secondStream := receiveBytes(int(intent.Identity.Binary.Size))
	preparer.result = secondBin
	preparer.closeChild = false
	writer.seal = domain.ProbeBinaryStageSeal{Intent: intent}
	writer.atCall = func() {
		writer.inputClosed = secondStream.closes > 0
		writer.binaryClosed = secondBin.closes > 0
	}
	got, err = svc.Receive(ctx, intent, secondStream)
	if err != nil || got.Intent != intent || writer.calls != 2 || secondBin.closes != 1 || secondStream.closes != 1 || !writer.inputClosed || writer.binaryClosed {
		t.Fatalf("second receive got (%+v, %v) writer=%d", got, err, writer.calls)
	}
}
