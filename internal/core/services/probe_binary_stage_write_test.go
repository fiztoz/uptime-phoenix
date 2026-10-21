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

// stageWritePanicWriter is a typed-nil trap: any method call panics.
type stageWritePanicWriter struct{}

func (*stageWritePanicWriter) StageBinary(context.Context, domain.ProbeBinaryStageIntent, ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	panic("typed-nil writer touched")
}

// stageWritePanicBinary is a typed-nil source trap. Pin, Open, and Close panic.
type stageWritePanicBinary struct{}

func (*stageWritePanicBinary) Pin() domain.ProbeBinaryPin { panic("typed-nil binary touched") }
func (*stageWritePanicBinary) Open(context.Context) (io.ReadCloser, error) {
	panic("typed-nil binary touched")
}
func (*stageWritePanicBinary) Close(context.Context) error { panic("typed-nil binary touched") }

// stageWritePrivateContext reports a non-sentinel diagnostic only after arming.
type stageWritePrivateContext struct {
	context.Context
	armed bool
}

func (c *stageWritePrivateContext) Err() error {
	if c.armed {
		return errors.New(stagePrivateMarker)
	}
	return c.Context.Err()
}

// stageWriteBinary records pin/open/close use and changes its pin after the
// first read so a second getter call cannot still satisfy an exact match.
type stageWriteBinary struct {
	pin    domain.ProbeBinaryPin
	pins   int
	opens  int
	closes int
}

func (b *stageWriteBinary) Pin() domain.ProbeBinaryPin {
	b.pins++
	copied := b.pin
	b.pin.Size++
	return copied
}

func (b *stageWriteBinary) Open(context.Context) (io.ReadCloser, error) {
	b.opens++
	panic("service opened borrowed binary")
}

func (b *stageWriteBinary) Close(context.Context) error {
	b.closes++
	panic("service closed borrowed binary")
}

type stageWriteWriter struct {
	calls  int
	ctx    context.Context
	intent domain.ProbeBinaryStageIntent
	binary ports.PreparedProbeBinary
	seal   domain.ProbeBinaryStageSeal
	err    error
	onCall func()
}

func (m *stageWriteWriter) StageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	m.calls++
	m.ctx = ctx
	m.intent = intent
	m.binary = binary
	if m.onCall != nil {
		m.onCall()
	}
	return m.seal, m.err
}

func assertStageWriteZero(t *testing.T, got domain.ProbeBinaryStageSeal, err, want error) {
	t.Helper()
	if err != want || got != (domain.ProbeBinaryStageSeal{}) {
		t.Fatalf("got (%+v, %v), want zero and %v", got, err, want)
	}
	if err != nil && (err.Error() != want.Error() || strings.Contains(err.Error(), stagePrivateMarker)) {
		t.Fatalf("diagnostic leaked or is not the bare sentinel: %q", err.Error())
	}
}

func TestProbeBinaryStageWriteServiceConstructor(t *testing.T) {
	if svc, err := NewProbeBinaryStageWriteService(nil); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("nil writer accepted", svc, err)
	}
	var typedNil *stageWritePanicWriter
	if svc, err := NewProbeBinaryStageWriteService(typedNil); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("typed-nil writer accepted", svc, err)
	}
	var typedIface ports.ProbeBinaryStageWriter = (*stageWritePanicWriter)(nil)
	if svc, err := NewProbeBinaryStageWriteService(typedIface); svc != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatal("typed-nil writer interface accepted", svc, err)
	}
	svc, err := NewProbeBinaryStageWriteService(&stageWriteWriter{})
	if err != nil || svc == nil {
		t.Fatal("valid writer refused", err)
	}
}

func TestProbeBinaryStageWriteServiceRefusesBeforeIO(t *testing.T) {
	intent := stageIntent()
	if !intent.Valid() || !stageDockerIntent().Identity.Valid() {
		t.Fatal("fixtures are not valid identities")
	}
	kinds := []string{
		"nil_service", "nil_writer", "typed_nil_writer", "nil_context", "typed_nil_context",
		"nil_binary", "typed_nil_binary", "docker_intent", "missing_owner", "missing_attempt",
		"bad_attempt", "pin_mismatch", "already_canceled", "already_expired", "wrapped_expired",
		"private_context", "invalid_and_canceled",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			request := intent
			source := &stageWriteBinary{pin: intent.Identity.Binary}
			writer := &stageWriteWriter{seal: domain.ProbeBinaryStageSeal{Intent: intent}}
			svc, err := NewProbeBinaryStageWriteService(writer)
			if err != nil {
				t.Fatal(err)
			}
			callCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target := svc
			var binary ports.PreparedProbeBinary = source
			want := domain.ErrProbeBinaryStageWriteInput
			expectPin := true
			switch kind {
			case "nil_service":
				target = nil
				expectPin = false
			case "nil_writer":
				target = &ProbeBinaryStageWriteService{}
				expectPin = false
			case "typed_nil_writer":
				target = &ProbeBinaryStageWriteService{writer: (*stageWritePanicWriter)(nil)}
				expectPin = false
			case "nil_context":
				callCtx = nil
				expectPin = false
			case "typed_nil_context":
				var nilCtx *stagePanicContext
				callCtx = nilCtx
				expectPin = false
			case "nil_binary":
				binary = nil
				expectPin = false
			case "typed_nil_binary":
				binary = (*stageWritePanicBinary)(nil)
				expectPin = false
			case "docker_intent":
				request = stageDockerIntent()
				source.pin = request.Identity.Binary
			case "missing_owner":
				request.OwnerID = ""
			case "missing_attempt":
				request.AttemptID = ""
			case "bad_attempt":
				request.AttemptID = strings.ToUpper("abcdefab-cdef-4abc-8abc-abcdefabcdef")
			case "pin_mismatch":
				source.pin.Size = intent.Identity.Binary.Size + 1
			case "already_canceled":
				cancel()
				want = context.Canceled
			case "already_expired":
				expired, stop := context.WithDeadline(t.Context(), time.Unix(1, 0))
				defer stop()
				callCtx = expired
				want = context.DeadlineExceeded
			case "wrapped_expired":
				callCtx = &stageExpireContext{Context: t.Context(), expired: true}
				want = context.DeadlineExceeded
			case "private_context":
				callCtx = &stageWritePrivateContext{Context: t.Context(), armed: true}
			case "invalid_and_canceled":
				cancel()
				request.OwnerID = ""
			}
			got, err := target.Stage(callCtx, request, binary)
			assertStageWriteZero(t, got, err, want)
			if writer.calls != 0 {
				t.Fatal("refused request called writer", writer.calls)
			}
			if expectPin && source.pins != 1 {
				t.Fatalf("pin calls=%d, want 1", source.pins)
			}
			if !expectPin && source.pins != 0 {
				t.Fatalf("pin calls=%d, want 0", source.pins)
			}
			if source.opens != 0 || source.closes != 0 {
				t.Fatal("service touched borrowed binary streams", source.opens, source.closes)
			}
		})
	}
}

func TestProbeBinaryStageWriteServiceStagesExactPin(t *testing.T) {
	intent := stageIntent()
	source := &stageWriteBinary{pin: intent.Identity.Binary}
	want := domain.ProbeBinaryStageSeal{Intent: intent}
	writer := &stageWriteWriter{seal: want}
	svc, err := NewProbeBinaryStageWriteService(writer)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	got, err := svc.Stage(ctx, intent, source)
	if err != nil || got != want || writer.calls != 1 || writer.ctx != ctx || writer.intent != intent || writer.binary != source {
		t.Fatalf("calls=%d err=%v got=%+v delegated=%+v binary=%v", writer.calls, err, got, writer.intent, writer.binary == source)
	}
	name := got.DirectoryName()
	if name != "uptime-phoenix-probe-"+intent.Identity.OperationID || strings.Contains(name, "/") {
		t.Fatal("seal invented a path", name)
	}
	if source.pins != 1 || source.opens != 0 || source.closes != 0 {
		t.Fatalf("borrowed source pins=%d opens=%d closes=%d", source.pins, source.opens, source.closes)
	}
	writer.seal.Intent.AttemptID = "44444444-4444-4444-8444-444444444444"
	source.pin.Size = 1
	if got != want {
		t.Fatal("returned seal aliased adapter state")
	}
}

func TestProbeBinaryStageWriteServiceRejectsReceipt(t *testing.T) {
	intent := stageIntent()
	foreign := intent
	foreign.AttemptID = "44444444-4444-4444-8444-444444444444"
	hidden := intent
	hidden.Identity.Image = stageDockerIntent().Identity.Image
	cases := []struct {
		name string
		seal domain.ProbeBinaryStageSeal
		err  error
	}{
		{name: "zero_nil", seal: domain.ProbeBinaryStageSeal{}},
		{name: "empty_intent", seal: domain.ProbeBinaryStageSeal{Intent: domain.ProbeBinaryStageIntent{}}},
		{name: "foreign_attempt", seal: domain.ProbeBinaryStageSeal{Intent: foreign}},
		{name: "hidden_image", seal: domain.ProbeBinaryStageSeal{Intent: hidden}},
		{name: "error_bearing_match", seal: domain.ProbeBinaryStageSeal{Intent: intent}, err: domain.ErrProbeBinaryStageWriteConflict},
		{name: "error_bearing_context", seal: domain.ProbeBinaryStageSeal{Intent: intent}, err: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &stageWriteBinary{pin: intent.Identity.Binary}
			writer := &stageWriteWriter{seal: tc.seal, err: tc.err}
			svc, err := NewProbeBinaryStageWriteService(writer)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Stage(t.Context(), intent, source)
			assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
			if writer.calls != 1 || writer.intent != intent || writer.binary != source || source.pins != 1 || source.opens != 0 || source.closes != 0 {
				t.Fatalf("calls=%d pins=%d delegated=%+v", writer.calls, source.pins, writer.intent)
			}
		})
	}
}

func TestProbeBinaryStageWriteServiceSanitizesLivePortErrors(t *testing.T) {
	intent := stageIntent()
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"bare_input", domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteInput},
		{"bare_conflict", domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteConflict},
		{"bare_unavailable", domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteUnavailable},
		{"bare_unknown", domain.ErrProbeBinaryStageWriteUnknown, domain.ErrProbeBinaryStageWriteUnknown},
		{"wrapped_input", fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteInput), domain.ErrProbeBinaryStageWriteInput},
		{"wrapped_conflict", fmt.Errorf("path /opt/secret: %w", domain.ErrProbeBinaryStageWriteConflict), domain.ErrProbeBinaryStageWriteConflict},
		{"wrapped_unavailable", errors.Join(domain.ErrProbeBinaryStageWriteUnavailable, errors.New(stagePrivateMarker)), domain.ErrProbeBinaryStageWriteUnavailable},
		{"wrapped_unknown", fmt.Errorf("%s: %w", stagePrivateMarker, domain.ErrProbeBinaryStageWriteUnknown), domain.ErrProbeBinaryStageWriteUnknown},
		{"wrapped_canceled", fmt.Errorf("%s: %w", stagePrivateMarker, context.Canceled), context.Canceled},
		{"wrapped_deadline", errors.Join(errors.New(stagePrivateMarker), context.DeadlineExceeded), context.DeadlineExceeded},
		{"ssh_not_allowlisted", domain.ErrProbeSSHInput, domain.ErrProbeBinaryStageWriteUnknown},
		{"inspection_not_allowlisted", domain.ErrProbeBinaryStageInspection, domain.ErrProbeBinaryStageWriteUnknown},
		{"private", errors.New(stagePrivateMarker), domain.ErrProbeBinaryStageWriteUnknown},
		{"joined_input_and_conflict", errors.Join(domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteInput), domain.ErrProbeBinaryStageWriteInput},
		{"joined_conflict_and_unavailable", errors.Join(domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteConflict), domain.ErrProbeBinaryStageWriteConflict},
		{"joined_context_and_conflict", errors.Join(context.Canceled, domain.ErrProbeBinaryStageWriteConflict), domain.ErrProbeBinaryStageWriteConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			source := &stageWriteBinary{pin: intent.Identity.Binary}
			writer := &stageWriteWriter{err: tc.err}
			svc, err := NewProbeBinaryStageWriteService(writer)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Stage(t.Context(), intent, source)
			assertStageWriteZero(t, got, err, tc.want)
			if writer.calls != 1 || source.opens != 0 || source.closes != 0 {
				t.Fatal("error path retried or touched streams", writer.calls, source.opens, source.closes)
			}
		})
	}
}

func TestProbeBinaryStageWriteServiceUnknownOutranksJoinedCauses(t *testing.T) {
	intent := stageIntent()
	cases := []error{
		errors.Join(domain.ErrProbeBinaryStageWriteConflict, domain.ErrProbeBinaryStageWriteUnknown),
		errors.Join(domain.ErrProbeBinaryStageWriteUnknown, context.Canceled),
		errors.Join(context.DeadlineExceeded, domain.ErrProbeBinaryStageWriteUnavailable, domain.ErrProbeBinaryStageWriteUnknown),
		fmt.Errorf("%s: %w", stagePrivateMarker, errors.Join(domain.ErrProbeBinaryStageWriteInput, domain.ErrProbeBinaryStageWriteUnknown)),
	}
	for i, portErr := range cases {
		t.Run(fmt.Sprintf("case_%d", i), func(t *testing.T) {
			source := &stageWriteBinary{pin: intent.Identity.Binary}
			writer := &stageWriteWriter{err: portErr}
			svc, err := NewProbeBinaryStageWriteService(writer)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Stage(t.Context(), intent, source)
			assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
			if writer.calls != 1 {
				t.Fatal(writer.calls)
			}
		})
	}
}

func TestProbeBinaryStageWriteServiceRevokesAfterAttempt(t *testing.T) {
	intent := stageIntent()
	success := domain.ProbeBinaryStageSeal{Intent: intent}
	t.Run("cancel_overrides_success", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		source := &stageWriteBinary{pin: intent.Identity.Binary}
		writer := &stageWriteWriter{seal: success, onCall: cancel}
		svc, err := NewProbeBinaryStageWriteService(writer)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Stage(ctx, intent, source)
		assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || writer.ctx != ctx {
			t.Fatal("service wrapped or skipped the caller context", writer.calls)
		}
	})
	t.Run("cancel_overrides_conflict", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		source := &stageWriteBinary{pin: intent.Identity.Binary}
		writer := &stageWriteWriter{err: domain.ErrProbeBinaryStageWriteConflict, onCall: cancel}
		svc, err := NewProbeBinaryStageWriteService(writer)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Stage(ctx, intent, source)
		assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
	})
	t.Run("deadline_overrides_success", func(t *testing.T) {
		base := &stageExpireContext{Context: t.Context()}
		source := &stageWriteBinary{pin: intent.Identity.Binary}
		writer := &stageWriteWriter{seal: success, onCall: func() { base.expired = true }}
		svc, err := NewProbeBinaryStageWriteService(writer)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Stage(base, intent, source)
		assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 || writer.ctx != context.Context(base) {
			t.Fatal("service wrapped or skipped the caller context", writer.calls)
		}
	})
	t.Run("private_context_overrides_success", func(t *testing.T) {
		base := &stageWritePrivateContext{Context: t.Context()}
		source := &stageWriteBinary{pin: intent.Identity.Binary}
		writer := &stageWriteWriter{seal: success, onCall: func() { base.armed = true }}
		svc, err := NewProbeBinaryStageWriteService(writer)
		if err != nil {
			t.Fatal(err)
		}
		got, err := svc.Stage(base, intent, source)
		assertStageWriteZero(t, got, err, domain.ErrProbeBinaryStageWriteUnknown)
		if writer.calls != 1 {
			t.Fatal(writer.calls)
		}
	})
}
