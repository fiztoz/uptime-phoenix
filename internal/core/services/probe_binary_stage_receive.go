package services

import (
	"context"
	"errors"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeBinaryStageReceiveService accepts one untrusted raw stream for a typed
// systemd staging intent. It borrows the writer and the parent that writer
// already holds. A seal is not host, journal, cleanup, or install authority.
type ProbeBinaryStageReceiveService struct {
	preparer ports.ProbeBinaryPreparer
	stage    *ProbeBinaryStageWriteService
}

// probeBinaryStageReceiveSafeErrors are the bare pre-stage sentinels. Storage
// and outcome-unknown are applied before this list so cleanup and mixed stage
// causes keep their required priority. Anything else is artifact storage.
var probeBinaryStageReceiveSafeErrors = []error{
	domain.ErrProbeBinaryStageWriteInput,
	domain.ErrProbeBinaryStageWriteConflict,
	domain.ErrProbeBinaryStageWriteUnavailable,
	domain.ErrProbeArtifactInput,
	domain.ErrProbeArtifactSize,
	domain.ErrProbeArtifactDigest,
	domain.ErrProbeArtifactMetadata,
	domain.ErrProbeArtifactClosed,
	context.Canceled,
	context.DeadlineExceeded,
}

// NewProbeBinaryStageReceiveService binds a preparer and the existing local
// stage service. A nil or typed-nil port is refused without a method call.
func NewProbeBinaryStageReceiveService(p ports.ProbeBinaryPreparer, w ports.ProbeBinaryStageWriter) (*ProbeBinaryStageReceiveService, error) {
	if probeBinaryStageNil(p) || probeBinaryStageNil(w) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	stage, err := NewProbeBinaryStageWriteService(w)
	if err != nil {
		return nil, err
	}
	return &ProbeBinaryStageReceiveService{preparer: p, stage: stage}, nil
}

// Receive owns a non-typed-nil input on every return, including refused input.
// It prepares one bounded child, requires real outer EOF, finishes the input,
// then delegates one existing Stage call. Mutation is not attempted before
// that boundary. Every failure returns a zero seal and a bare sentinel.
func (s *ProbeBinaryStageReceiveService) Receive(ctx context.Context, intent domain.ProbeBinaryStageIntent, input io.ReadCloser) (domain.ProbeBinaryStageSeal, error) {
	// The parameter is already a value copy; keep using that captured intent.
	if err := probeBinaryStageReceiveReady(s, ctx, intent, input); err != nil {
		return probeBinaryStageReceiveRefuseInput(input, err)
	}
	src, err := newProbeBinaryStageReceiveSource(ctx, input, intent.Identity.Binary.Size)
	if err != nil || src == nil {
		if err == nil {
			err = domain.ErrProbeArtifactStorage
		}
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageReceivePreError(err, nil, nil)
	}
	return s.receive(ctx, intent, src)
}

func probeBinaryStageReceiveReady(s *ProbeBinaryStageReceiveService, ctx context.Context, intent domain.ProbeBinaryStageIntent, input io.ReadCloser) error {
	if s == nil || s.stage == nil || probeBinaryStageNil(s.preparer) || probeBinaryStageNil(s.stage.writer) ||
		probeBinaryStageNil(ctx) || probeBinaryStageNil(input) || !intent.Valid() {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageReceiveContextError(err)
	}
	return nil
}

func probeBinaryStageReceiveRefuseInput(input io.ReadCloser, err error) (domain.ProbeBinaryStageSeal, error) {
	if closeErr := probeBinaryStageReceiveCloseInput(input); closeErr != nil {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeArtifactStorage
	}
	return domain.ProbeBinaryStageSeal{}, err
}

func probeBinaryStageReceiveContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeBinaryStageWriteInput
	}
}

// probeBinaryStageReceivePreError keeps one bare pre-stage cause. Any owned
// input or snapshot cleanup failure is storage and outranks cancellation.
// Outcome-unknown outranks other mixed stage causes. Private diagnostics never
// pass through.
func probeBinaryStageReceivePreError(reason, finishErr, cleanupErr error) error {
	if finishErr != nil || cleanupErr != nil || errors.Is(reason, domain.ErrProbeArtifactStorage) {
		return domain.ErrProbeArtifactStorage
	}
	if reason == nil || errors.Is(reason, domain.ErrProbeBinaryStageWriteUnknown) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	for _, safe := range probeBinaryStageReceiveSafeErrors {
		if errors.Is(reason, safe) {
			return safe
		}
	}
	return domain.ErrProbeArtifactStorage
}

type probeBinaryStageReceiveHold struct {
	binary ports.PreparedProbeBinary
	closed bool
}

// dispose closes a real snapshot at most once. Typed-nil values are not
// dispatched. The cleanup context is detached so caller cancellation cannot
// itself become the close result; a real close failure is storage.
func (h *probeBinaryStageReceiveHold) dispose(ctx context.Context) error {
	if h == nil || h.closed || probeBinaryStageNil(h.binary) {
		return nil
	}
	h.closed = true
	cleanup := context.Background()
	if !probeBinaryStageNil(ctx) {
		cleanup = context.WithoutCancel(ctx)
	}
	if err := h.binary.Close(cleanup); err != nil {
		return domain.ErrProbeArtifactStorage
	}
	return nil
}

type probeBinaryStageReceiveFinish struct {
	src      *probeBinaryStageReceiveSource
	finished bool
	err      error
}

func (f *probeBinaryStageReceiveFinish) finish() error {
	if f == nil || f.src == nil {
		return domain.ErrProbeArtifactStorage
	}
	if !f.finished {
		f.finished = true
		if err := f.src.Finish(); err != nil {
			f.err = domain.ErrProbeArtifactStorage
		}
	}
	return f.err
}

func (s *ProbeBinaryStageReceiveService) receive(ctx context.Context, intent domain.ProbeBinaryStageIntent, src *probeBinaryStageReceiveSource) (domain.ProbeBinaryStageSeal, error) {
	child := src.Child()
	prepared, prepErr := probeBinaryStageReceivePrepare(s.preparer, ctx, intent.Identity.Binary, child)
	hold := &probeBinaryStageReceiveHold{binary: prepared}
	done := &probeBinaryStageReceiveFinish{src: src}
	if reason := probeBinaryStageReceivePreparationError(ctx, intent, prepared, prepErr, src.Complete()); reason != nil {
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageReceiveAbandon(ctx, done, hold, reason)
	}
	if err := src.ConfirmEOF(); err != nil {
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageReceiveAbandon(ctx, done, hold, err)
	}
	if err := done.finish(); err != nil {
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageReceiveAbandon(ctx, done, hold, err)
	}
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageReceiveAbandon(ctx, done, hold, probeBinaryStageReceiveContextError(err))
	}
	seal, stageErr := s.stage.Stage(ctx, intent, prepared)
	return probeBinaryStageReceiveAfterStage(ctx, intent, seal, stageErr, hold.dispose(ctx))
}

func probeBinaryStageReceivePrepare(preparer ports.ProbeBinaryPreparer, ctx context.Context, pin domain.ProbeBinaryPin, child io.ReadCloser) (ports.PreparedProbeBinary, error) {
	if probeBinaryStageNil(child) {
		return nil, domain.ErrProbeArtifactStorage
	}
	prepared, err := preparer.PrepareBinary(ctx, pin, child)
	if closeErr := child.Close(); closeErr != nil {
		return prepared, domain.ErrProbeArtifactStorage
	}
	return prepared, err
}

func probeBinaryStageReceivePreparationError(ctx context.Context, intent domain.ProbeBinaryStageIntent, prepared ports.PreparedProbeBinary, prepErr error, complete bool) error {
	if prepErr != nil {
		return prepErr
	}
	if probeBinaryStageNil(prepared) {
		return domain.ErrProbeArtifactMetadata
	}
	pin := prepared.Pin()
	if !pin.Valid() || pin != intent.Identity.Binary {
		return domain.ErrProbeArtifactMetadata
	}
	if !complete {
		return domain.ErrProbeArtifactSize
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageReceiveContextError(err)
	}
	return nil
}

func probeBinaryStageReceiveAbandon(ctx context.Context, done *probeBinaryStageReceiveFinish, hold *probeBinaryStageReceiveHold, reason error) error {
	finishErr := done.finish()
	cleanupErr := hold.dispose(ctx)
	return probeBinaryStageReceivePreError(reason, finishErr, cleanupErr)
}

// probeBinaryStageReceiveAfterStage runs only after Stage was invoked. Cleanup
// failure, caller cancellation, and a foreign, malformed, or error-bearing
// seal are outcome-unknown. A live explicit stage refusal keeps the existing
// stage allowlist. Success is the exact intent seal after snapshot cleanup.
func probeBinaryStageReceiveAfterStage(ctx context.Context, intent domain.ProbeBinaryStageIntent, seal domain.ProbeBinaryStageSeal, stageErr, cleanupErr error) (domain.ProbeBinaryStageSeal, error) {
	if cleanupErr != nil || probeBinaryStageNil(ctx) || ctx.Err() != nil {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
	}
	if stageErr != nil {
		if seal != (domain.ProbeBinaryStageSeal{}) {
			return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
		}
		return domain.ProbeBinaryStageSeal{}, probeBinaryStageWriteSafeError(stageErr)
	}
	copied := seal
	if !copied.Valid() || copied.Intent != intent {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteUnknown
	}
	return copied, nil
}
