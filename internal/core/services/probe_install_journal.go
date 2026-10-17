package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeInstallJournalService is the use-case facade over the durable
// initial-install intent and resource journal. It performs no execution and no
// external I/O: every method is one fenced repository call wrapped in strict
// validation of the caller's request and of the complete returned graph.
// There is no installed/success outcome and no authority beyond the stored
// journal admission; a fresh action grant is returned only when the commit
// itself is acknowledged on a live context.
type ProbeInstallJournalService struct {
	operations ports.ProbeInstallOperationRepository
	resources  ports.ProbeInstallResourceJournal
}

// NewProbeInstallJournalService binds the two journal ports. The facade owns
// no state and starts no work: it only validates, delegates and normalizes.
func NewProbeInstallJournalService(operations ports.ProbeInstallOperationRepository, resources ports.ProbeInstallResourceJournal) (*ProbeInstallJournalService, error) {
	if operations == nil || resources == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeInstallJournalService{operations: operations, resources: resources}, nil
}

// Create records one complete installation intent and its derived canonical
// resource graph atomically. An exact identity retry returns the original
// receipt (created=false) with its original owner and never replenishes
// authority; a created=true receipt is admitted under the caller's owner in
// the queued state. Changed identity surfaces the repository conflict.
func (s *ProbeInstallJournalService) Create(ctx context.Context, request domain.ProbeInstallIdentity, ownerID string) (*domain.ProbeInstallOperation, bool, error) {
	if ctx == nil {
		return nil, false, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !request.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, false, domain.ErrValidation
	}
	operation, created, err := s.operations.CreateInstall(ctx, request, ownerID)
	if err != nil {
		return nil, false, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		// A failed or canceled post-commit acknowledgement is never a fresh
		// grant: the caller retries and receives the original receipt.
		return nil, false, err
	}
	if operation == nil || !operation.Valid() || operation.Identity != request ||
		(created && (operation.OwnerID != ownerID || operation.State != domain.ProbeInstallQueued)) {
		return nil, false, domain.ErrInternal
	}
	// created=false preserves the original owner untouched, whatever owner the
	// retrying caller passed: no owner takeover or rebind ever exists here.
	return installCopyOperation(operation), created, nil
}

// Observe reads the durable operation and its complete resource graph. It
// never creates, dispatches, renews or reconciles anything on its own.
func (s *ProbeInstallJournalService) Observe(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) {
		return nil, domain.ErrValidation
	}
	operation, err := s.operations.ObserveInstall(ctx, operationID)
	if err != nil {
		return nil, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, err
	}
	return installCopyOperation(operation), nil
}

// Begin is the single queued-to-running transition under the stored owner.
// A successful return is the dispatch grant for that operation; a second
// begin can never dispatch again. The returned graph must show the admitted
// owner running, so a stale or corrupt acknowledgement is never a grant.
func (s *ProbeInstallJournalService) Begin(ctx context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, domain.ErrValidation
	}
	operation, err := s.operations.BeginInstall(ctx, operationID, ownerID)
	if err != nil {
		return nil, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, err
	}
	if operation.OwnerID != ownerID || operation.State != domain.ProbeInstallRunning || operation.ErrorCode != "" {
		return nil, domain.ErrInternal
	}
	return installCopyOperation(operation), nil
}

// Cancel persists cancellation intent (or the terminal cancel of never-begun
// work) on any replica. It never claims remote execution stopped: the owning
// executor must still join its work before a terminal cancel is recorded.
// Terminal receipts are returned unchanged.
func (s *ProbeInstallJournalService) Cancel(ctx context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) {
		return nil, domain.ErrValidation
	}
	operation, err := s.operations.RequestInstallCancellation(ctx, operationID)
	if err != nil {
		return nil, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, err
	}
	switch operation.State {
	case domain.ProbeInstallCancelRequested, domain.ProbeInstallCanceled,
		domain.ProbeInstallFailed, domain.ProbeInstallInterrupted:
		return installCopyOperation(operation), nil
	default:
		// A successful cancellation request can never leave active work
		// states untouched or invent an installed/success state.
		return nil, domain.ErrInternal
	}
}

// Finish records exactly one owner outcome: failed, canceled or the owner's
// own interruption. There is no success/installed outcome. The stored graph
// must show the joined owner and the exact requested terminal outcome, so a
// stale, repaired or fabricated acknowledgement is never accepted.
func (s *ProbeInstallJournalService) Finish(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) || !outcome.Valid() {
		return nil, domain.ErrValidation
	}
	operation, err := s.operations.FinishInstall(ctx, operationID, ownerID, outcome)
	if err != nil {
		return nil, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, err
	}
	if operation.OwnerID != ownerID || !operation.Terminal() ||
		operation.State != outcome.State || operation.ErrorCode != outcome.ErrorCode {
		return nil, domain.ErrInternal
	}
	return installCopyOperation(operation), nil
}

// ReserveResource binds one canonical resource to one canonical attempt UUID
// under the stored owner. created=true is a grant for exactly one external
// invocation and requires the admitted owner running with that resource
// committed in_flight. created=false never grants anything: exact retries,
// including historical outcomes after expiry, must not be re-executed.
func (s *ProbeInstallJournalService) ReserveResource(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	if ctx == nil {
		return nil, false, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!installValidResourceKind(kind) || !domain.ValidProbeOperationID(attemptID) {
		return nil, false, domain.ErrValidation
	}
	operation, created, err := s.resources.ReserveInstallResource(ctx, operationID, ownerID, kind, attemptID)
	if err != nil {
		return nil, false, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		// A canceled post-commit acknowledgement never returns a fresh grant.
		return nil, false, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, false, err
	}
	resource, ok := installFindResource(operation, kind)
	if !ok || operation.OwnerID != ownerID || resource.AttemptID != attemptID {
		return nil, false, domain.ErrInternal
	}
	if created && (operation.State != domain.ProbeInstallRunning ||
		resource.State != domain.ProbeInstallResourceInFlight) {
		// Only an exact in-flight intent on the running, admitted owner is a
		// fresh action grant.
		return nil, false, domain.ErrInternal
	}
	// created=false is returned exactly as received and never authorizes
	// another invocation, whatever the recorded state or history.
	return installCopyOperation(operation), created, nil
}

// RecordResource reports one joined resource action for an exact stored
// attempt under the stored owner. Applied/not_started/unknown reports are
// immutable history: the stored outcome must match the request exactly, and a
// late or altered acknowledgement is rejected rather than repaired.
func (s *ProbeInstallJournalService) RecordResource(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!installValidResourceKind(kind) || !domain.ValidProbeOperationID(attemptID) || !outcome.Valid() {
		return nil, domain.ErrValidation
	}
	operation, err := s.resources.RecordInstallResource(ctx, operationID, ownerID, kind, attemptID, outcome)
	if err != nil {
		return nil, installJournalError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := installValidateOperation(operation, operationID); err != nil {
		return nil, err
	}
	resource, ok := installFindResource(operation, kind)
	if !ok || operation.OwnerID != ownerID || resource.AttemptID != attemptID ||
		resource.State != outcome.State || resource.ErrorCode != outcome.ErrorCode {
		return nil, domain.ErrInternal
	}
	return installCopyOperation(operation), nil
}

// installJournalError maps every repository failure onto the fixed public
// sentinels. Not-found, conflict and context causes survive unchanged; repo
// corruption and unknown raw errors (which may carry private diagnostics)
// collapse to the fixed domain.ErrInternal without leaking their text.
func installJournalError(err error) error {
	switch {
	case errors.Is(err, ports.ErrNotFound):
		return ports.ErrNotFound
	case errors.Is(err, ports.ErrConflict):
		return ports.ErrConflict
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrInternal
	}
}

// installValidateOperation checks that a receipt exists, is a complete valid
// graph and is the requested operation before any caller sees it.
func installValidateOperation(operation *domain.ProbeInstallOperation, operationID string) error {
	if operation == nil || !operation.Valid() || operation.Identity.OperationID != operationID {
		return domain.ErrInternal
	}
	return nil
}

// installCopyOperation returns a defensive copy whose resource slice cannot be
// mutated through (or corrupted by) the caller or the repository receipt.
func installCopyOperation(operation *domain.ProbeInstallOperation) *domain.ProbeInstallOperation {
	copied := *operation
	copied.Resources = append([]domain.ProbeInstallResource(nil), operation.Resources...)
	return &copied
}

// installFindResource locates the one canonical resource of the given kind.
func installFindResource(operation *domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResource, bool) {
	for _, resource := range operation.Resources {
		if resource.Spec.Kind == kind {
			return resource, true
		}
	}
	return domain.ProbeInstallResource{}, false
}

// installValidResourceKind accepts only the frozen layout v1 resource kinds
// derived by ProbeInstallIdentity.Resources(); references are never
// caller-controlled.
func installValidResourceKind(kind string) bool {
	switch kind {
	case "data_directory", "artifact_directory", "unit_file", "service", "image_cache", "container":
		return true
	default:
		return false
	}
}
