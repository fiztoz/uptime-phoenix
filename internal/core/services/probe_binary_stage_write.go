package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeBinaryStageWriteService checks one borrowed binary against a systemd
// staging intent, then delegates a single fresh local stage attempt. It does
// not open or close the binary, and a seal is not host, journal, or cleanup
// authority.
type ProbeBinaryStageWriteService struct {
	writer ports.ProbeBinaryStageWriter
}

// probeBinaryStageWriteSafeErrors is the post-attempt allowlist used only while
// the caller context is still live. Unknown is first so a joined cause cannot
// be reported as a definite refusal, conflict, or cancellation. Context
// sentinels here are a pre-create port refusal, not caller cancellation after
// a potentially effectful call.
var probeBinaryStageWriteSafeErrors = []error{
	domain.ErrProbeBinaryStageWriteUnknown,
	domain.ErrProbeBinaryStageWriteInput,
	domain.ErrProbeBinaryStageWriteConflict,
	domain.ErrProbeBinaryStageWriteUnavailable,
	context.Canceled,
	context.DeadlineExceeded,
}

// NewProbeBinaryStageWriteService binds a local staging writer.
// A nil or typed-nil writer is refused so no method is called on an absent adapter.
func NewProbeBinaryStageWriteService(p ports.ProbeBinaryStageWriter) (*ProbeBinaryStageWriteService, error) {
	if probeBinaryStageNil(p) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	return &ProbeBinaryStageWriteService{writer: p}, nil
}

// probeBinaryStageWriteContextError keeps only the two context sentinels before
// a writer call. Any other context diagnostic is input refusal, never private
// text and never an outcome-unknown claim for work that did not start.
func probeBinaryStageWriteContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeBinaryStageWriteInput
	}
}

// probeBinaryStageWriteSafeError maps a live port diagnostic to one bare
// allowlisted cause. Unknown wins when several causes match; anything else is
// outcome-unknown because the attempt may already have mutated the parent.
func probeBinaryStageWriteSafeError(err error) error {
	for _, safe := range probeBinaryStageWriteSafeErrors {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeBinaryStageWriteUnknown
}

// Stage copies the source pin once and requires that immutable value to equal
// the intent before delegation. The binary is borrowed: this method never
// opens or closes it. After the single writer call, caller cancellation, a
// non-exact or error-bearing receipt, or a private diagnostic is bare
// outcome-unknown. A live caller may still receive one sanitized pre-create
// port error.
func (s *ProbeBinaryStageWriteService) Stage(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	if s == nil || probeBinaryStageNil(s.writer) || probeBinaryStageNil(ctx) || probeBinaryStageNil(binary) {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteInput
	}
	pin := binary.Pin()
	if !intent.Valid() || pin != intent.Identity.Binary {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageWriteContextError(err)
	}
	seal, err := s.writer.StageBinary(ctx, intent, binary)
	if ctx.Err() != nil {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
	}
	copied := seal
	if err != nil {
		if copied != (domain.ProbeBinaryStageSeal{}) {
			return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
		}
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageWriteSafeError(err)
	}
	if !copied.Valid() || copied.Intent != intent {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
	}
	return copied, nil
}
