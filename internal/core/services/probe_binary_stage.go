package services

import (
	"context"
	"errors"
	"reflect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeBinaryStageInspectionService validates a proposed systemd staging intent
// and accepts only one read-only observation of that exact intent. A match is
// not a writer, upload, importer, host fence, journal grant, or installation.
type ProbeBinaryStageInspectionService struct {
	inspector ports.ProbeBinaryStageInspector
}

// probeBinaryStageSafeErrors is the complete allowlist of bare port diagnostics
// this service may return. Unknown and private diagnostics become inspection
// unavailable. Context sentinels are included so a wrapped cancellation does
// not escape with adapter text.
var probeBinaryStageSafeErrors = []error{
	domain.ErrProbeBinaryStageInput,
	domain.ErrProbeBinaryStageInspection,
	domain.ErrProbeSSHInput,
	domain.ErrProbeSSHTrust,
	domain.ErrProbeSSHDestination,
	domain.ErrProbeSSHConnect,
	context.Canceled,
	context.DeadlineExceeded,
}

// NewProbeBinaryStageInspectionService binds a read-only staging inspector.
// A nil or typed-nil inspector is refused so no method is called on an absent adapter.
func NewProbeBinaryStageInspectionService(p ports.ProbeBinaryStageInspector) (*ProbeBinaryStageInspectionService, error) {
	if probeBinaryStageNil(p) {
		return nil, domain.ErrProbeBinaryStageInput
	}
	return &ProbeBinaryStageInspectionService{inspector: p}, nil
}

// probeBinaryStageNil reports a nil interface or an interface holding a nil
// reference. It never invokes methods, so a typed-nil context or inspector is
// refused without the panic its methods would raise.
func probeBinaryStageNil(v any) bool {
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

// probeBinaryStageSafeError maps a port diagnostic to one bare allowlisted
// cause. The first matching sentinel wins; anything else is inspection unavailable.
func probeBinaryStageSafeError(err error) error {
	for _, safe := range probeBinaryStageSafeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeBinaryStageInspection
}

// probeBinaryStageContextError keeps only the two context sentinels. A custom
// context error is not a port diagnostic and must not leak private text.
func probeBinaryStageContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeBinaryStageInspection
	}
}

// Inspect checks the caller, intent, and context before one inspector call.
// Cancellation discovered after that call revokes any observation. Every
// refusal returns the zero observation and a bare allowlisted error.
func (s *ProbeBinaryStageInspectionService) Inspect(ctx context.Context, intent domain.ProbeBinaryStageIntent) (domain.ProbeBinaryStageObservation, error) {
	if s == nil || probeBinaryStageNil(s.inspector) || probeBinaryStageNil(ctx) || !intent.Valid() {
		return domain.ProbeBinaryStageObservation{}, domain.ErrProbeBinaryStageInput
	}
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageObservation{}, probeBinaryStageContextError(err)
	}
	observed, err := s.inspector.InspectBinaryStage(ctx, intent)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return domain.ProbeBinaryStageObservation{}, probeBinaryStageContextError(ctxErr)
	}
	if err != nil {
		return domain.ProbeBinaryStageObservation{}, probeBinaryStageSafeError(err)
	}
	copied := observed
	if !copied.Valid() || copied.Intent != intent {
		return domain.ProbeBinaryStageObservation{}, domain.ErrProbeBinaryStageInspection
	}
	return copied, nil
}
