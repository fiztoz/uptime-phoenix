package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Process-local execution bounds for asynchronous SSH host preflights. The
// composition root passes 4; only values in [1,16] are admitted.
const (
	probePreflightMinCapacity   = 1
	probePreflightMaxCapacity   = 16
	probePreflightPollInterval  = time.Second
	probePreflightFinalWriteTTL = 5 * time.Second
	probePreflightInspectTTL    = 30 * time.Second
	probePreflightPollTTL       = 2 * time.Second
)

// probePreflightInspectResult carries one joined inspector return. The raw
// error never leaves this package unmapped and is never persisted.
type probePreflightInspectResult struct {
	facts *domain.ProbeHostPreflight
	err   error
}

// ProbePreflightOperationService executes admitted SSH host-preflight discovery
// inside one process with bounded concurrency. Every lifecycle decision is
// fenced by the durable repository, the only authority for owner/state/expiry.
// No borrowed key buffer or raw output is retained. The ephemeral parsed signer
// lives only in the per-job inspector; no credential is durable.
type ProbePreflightOperationService struct {
	repo     ports.ProbePreflightRepository
	factory  ports.ProbeHostInspectorFactory
	ownerID  string
	capacity int

	rootCtx    context.Context
	rootCancel context.CancelFunc

	mu        sync.Mutex
	closed    bool
	active    int
	tasks     sync.WaitGroup
	pollEvery time.Duration
}

// NewProbePreflightOperationService binds a durable preflight repository, a
// transient-key inspector factory and the composition-generated owner UUID that
// fences every write this executor performs. ctx is the root lifetime: canceling
// it (or calling Close) stops admission and cancels all tracked executions.
func NewProbePreflightOperationService(ctx context.Context, repo ports.ProbePreflightRepository, factory ports.ProbeHostInspectorFactory, ownerID string, capacity int) (*ProbePreflightOperationService, error) {
	if ctx == nil || repo == nil || factory == nil || !domain.ValidProbeOperationID(ownerID) ||
		capacity < probePreflightMinCapacity || capacity > probePreflightMaxCapacity {
		return nil, domain.ErrValidation
	}
	rootCtx, rootCancel := context.WithCancel(ctx)
	return &ProbePreflightOperationService{
		repo:       repo,
		factory:    factory,
		ownerID:    ownerID,
		capacity:   capacity,
		rootCtx:    rootCtx,
		rootCancel: rootCancel,
		pollEvery:  probePreflightPollInterval,
	}, nil
}

// Submit admits one transient SSH preflight request and dispatches execution
// only after CreatePreflight commits a brand-new row (created=true). An exact
// identity retry returns the original receipt — original owner, deadline and
// outcome — without a second dispatch, renewal or owner rebind. A changed
// selector, actor or public key surfaces the repository conflict untouched.
func (s *ProbePreflightOperationService) Submit(ctx context.Context, request domain.ProbePreflightSubmission) (*domain.ProbePreflightOperation, error) {
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(request.OperationID) || request.RequestedBy <= 0 {
		return nil, domain.ErrValidation
	}
	if !request.Host.Valid() || !domain.ValidSSHKeyFingerprint(request.HostKeyFingerprint) ||
		len(request.PrivateKey) == 0 || len(request.PrivateKey) > 64<<10 {
		return nil, domain.ErrProbeSSHInput
	}
	// The bounded slot is reserved before any durable work is created; every
	// pre-dispatch failure below releases it again.
	admissionErr := s.reserve()
	if admissionErr != nil && !errors.Is(admissionErr, domain.ErrProbePreflightBusy) {
		return nil, admissionErr
	}
	reserved := admissionErr == nil
	dispatched := false
	defer func() {
		if reserved && !dispatched {
			s.release()
		}
	}()
	// Shutdown also cancels an in-progress admission, including a blocked DB
	// call. No request can hold Close open through an unbounded admission.
	admitCtx, cancelAdmission := context.WithTimeout(ctx, probePreflightFinalWriteTTL)
	stop := context.AfterFunc(s.rootCtx, cancelAdmission)
	defer stop()
	defer cancelAdmission()
	// The private key is borrowed for synchronous parsing only. It is passed
	// straight to the factory and never stored on the service, the identity or
	// any goroutine-scoped state.
	inspector, clientFingerprint, err := s.factory.NewInspector(admitCtx, request.PrivateKey, request.HostKeyFingerprint)
	if err != nil {
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbePreflightClosed
		}
		return nil, fmt.Errorf("parsing transient SSH preflight key: %w", err)
	}
	if inspector == nil {
		return nil, domain.ErrInternal
	}
	identity := domain.ProbePreflightIdentity{
		OperationID:          request.OperationID,
		Host:                 request.Host,
		HostKeyFingerprint:   request.HostKeyFingerprint,
		ClientKeyFingerprint: clientFingerprint,
		RequestedBy:          request.RequestedBy,
	}
	if !identity.Valid() {
		// Inputs above are already validated: a bad public fingerprint is
		// a factory contract failure, not a malformed client submission.
		return nil, domain.ErrInternal
	}
	if !reserved {
		// Saturation must not break idempotency for already committed work.
		// Verify the complete identity, including the PUBLIC key, without
		// creating work or reserving/dispatching another executor.
		current, err := s.repo.ObservePreflight(admitCtx, identity.OperationID)
		if errors.Is(err, ports.ErrNotFound) {
			return nil, domain.ErrProbePreflightBusy
		}
		if err != nil {
			return nil, preflightRepositoryError("observing saturated preflight retry", err)
		}
		if current == nil || !current.Valid() {
			return nil, domain.ErrInternal
		}
		if current.Identity != identity {
			return nil, ports.ErrConflict
		}
		return current, nil
	}
	operation, created, err := s.repo.CreatePreflight(admitCtx, identity, s.ownerID)
	if err != nil {
		// Ambiguous or failed inserts never dispatch SSH; any row that did
		// commit without an executor honestly expires to interrupted.
		if s.rootCtx.Err() != nil && ctx.Err() == nil {
			return nil, domain.ErrProbePreflightClosed
		}
		return nil, preflightRepositoryError("creating SSH preflight operation", err)
	}
	if operation == nil || !operation.Valid() || operation.Identity != identity ||
		(created && (operation.OwnerID != s.ownerID || operation.State != domain.ProbePreflightQueued)) {
		return nil, domain.ErrInternal
	}
	if !created {
		return operation, nil
	}
	dispatched = true
	go s.execute(s.rootCtx, identity, inspector)
	return operation, nil
}

// Observe reads the durable receipt for one operation and lets the repository
// reconcile any hard-expired nonterminal row to interrupted. It never creates,
// dispatches or renews work.
func (s *ProbePreflightOperationService) Observe(ctx context.Context, id string) (*domain.ProbePreflightOperation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	operation, err := s.repo.ObservePreflight(ctx, id)
	if err != nil {
		return nil, preflightRepositoryError("observing SSH preflight operation", err)
	}
	return operation, nil
}

// Cancel persists cancellation intent for one operation on any replica. It is
// never a claim that SSH work stopped: the owning executor observes the stored
// intent by polling, joins its inspector, and only then records canceled under
// the repository's fences. Terminal receipts are returned unchanged.
func (s *ProbePreflightOperationService) Cancel(ctx context.Context, id string) (*domain.ProbePreflightOperation, error) {
	if ctx == nil || !domain.ValidProbeOperationID(id) {
		return nil, domain.ErrValidation
	}
	operation, err := s.repo.RequestPreflightCancellation(ctx, id)
	if err != nil {
		return nil, preflightRepositoryError("requesting SSH preflight cancellation", err)
	}
	return operation, nil
}

// preflightRepositoryError preserves the cause while distinguishing corrupt
// stored state from input validation. Public methods validate inputs before
// calling the repository; its validation failures are internal/unavailable.
func preflightRepositoryError(action string, err error) error {
	if errors.Is(err, domain.ErrValidation) {
		err = errors.Join(domain.ErrInternal, err)
	}
	return fmt.Errorf("%s: %w", action, err)
}

// Close stops admission, cancels the root lifetime and waits until every
// tracked execution has joined its inspector and attempted its final write.
// A bound context limits the wait; ctx expiry reports an incomplete shutdown
// rather than pretending the tasks were joined. Close is idempotent.
func (s *ProbePreflightOperationService) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	// Admission checks this flag under the same mutex that guards WaitGroup
	// admission, so no Add can race a started Wait.
	s.closed = true
	s.mu.Unlock()
	s.rootCancel()
	joined := make(chan struct{})
	go func() {
		s.tasks.Wait()
		close(joined)
	}()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("closing SSH preflight executor: %w", ctx.Err())
	}
}

// reserve admits one unit of bounded work while the executor is open. The
// WaitGroup increment happens under the admission mutex before Close can start
// waiting, which removes the Add/Wait race.
func (s *ProbePreflightOperationService) reserve() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rootCtx.Err() != nil {
		return domain.ErrProbePreflightClosed
	}
	if s.active >= s.capacity {
		return domain.ErrProbePreflightBusy
	}
	s.active++
	s.tasks.Add(1)
	return nil
}

// release returns one reserved slot and signals shutdown waiters. It runs only
// after the final write attempt (or immediately when dispatch never happened).
func (s *ProbePreflightOperationService) release() {
	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	s.tasks.Done()
}

// finalWriteContext is the independent bounded context used for every durable
// write attempted by an execution task. It stays usable after the root context
// or a submitting request has been canceled.
func (s *ProbePreflightOperationService) finalWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), probePreflightFinalWriteTTL)
}

// execute runs one admitted operation: fence the begin, then inspect, then
// persist exactly one outcome.
func (s *ProbePreflightOperationService) execute(ctx context.Context, identity domain.ProbePreflightIdentity, inspector ports.ProbeHostInspector) {
	defer s.release()
	beginCtx, cancel := context.WithTimeout(ctx, probePreflightFinalWriteTTL)
	_, err := s.repo.BeginPreflight(beginCtx, identity.OperationID, s.ownerID)
	cancel()
	if err != nil {
		s.handleBeginFailure(ctx, identity.OperationID, err)
		return
	}
	s.dispatchInspection(ctx, identity, inspector)
}

// handleBeginFailure reconciles a begin that never took effect. A cross-replica
// cancellation recorded while the operation was still queued may complete as
// canceled because the inspector genuinely never started. Shutdown records
// interruption; other failures leave the row for honest expiry reconciliation.
func (s *ProbePreflightOperationService) handleBeginFailure(ctx context.Context, opID string, beginErr error) {
	if !(errors.Is(beginErr, context.Canceled) && s.rootCtx.Err() != nil) {
		slog.ErrorContext(s.rootCtx, "SSH preflight execution begin failed", "operation_id", opID)
	}
	finalCtx, cancel := s.finalWriteContext(ctx)
	defer cancel()
	current, err := s.repo.ObservePreflight(finalCtx, opID)
	if err != nil {
		slog.ErrorContext(s.rootCtx, "SSH preflight reconciliation failed", "operation_id", opID)
		return
	}
	if current.Terminal() {
		return
	}
	outcome := domain.ProbePreflightOutcome{State: domain.ProbePreflightCanceled, ErrorCode: "canceled"}
	if current.State != domain.ProbePreflightCancelRequested {
		if s.rootCtx.Err() == nil {
			return
		}
		// Begin never dispatched an inspector, including an ambiguous begin.
		outcome = domain.ProbePreflightOutcome{State: domain.ProbePreflightInterrupted, ErrorCode: "interrupted"}
	}
	if _, err := s.repo.FinishPreflight(finalCtx, opID, s.ownerID, outcome); err != nil {
		slog.ErrorContext(s.rootCtx, "SSH preflight cancellation completion failed", "operation_id", opID)
	}
}

// dispatchInspection runs the real compatibility preflight concurrently with a
// durable poll loop and always joins the inspector before completing.
func (s *ProbePreflightOperationService) dispatchInspection(ctx context.Context, identity domain.ProbePreflightIdentity, inspector ports.ProbeHostInspector) {
	opID := identity.OperationID
	execCtx, execCancel := context.WithTimeout(ctx, probePreflightInspectTTL)
	defer execCancel()
	resultCh := make(chan probePreflightInspectResult, 1)
	preflight := NewProbeHostPreflightService(inspector)
	go func() {
		facts, err := preflight.Inspect(execCtx, identity.Host)
		resultCh <- probePreflightInspectResult{facts: facts, err: err}
	}()
	every := s.pollEvery
	if every <= 0 {
		every = probePreflightPollInterval
	}
	timer := time.NewTimer(every)
	defer timer.Stop()
	cancelObserved := false
	for {
		select {
		case result := <-resultCh:
			// The inspector already returned; a root cancellation observed in
			// the same instant must not relabel a passing result.
			s.complete(ctx, opID, cancelObserved, s.rootCtx.Err() != nil, result)
			return
		case <-s.rootCtx.Done():
			execCancel()
			result := <-resultCh // join before any interrupted persistence
			s.complete(ctx, opID, cancelObserved, true, result)
			return
		case <-timer.C:
			// At most once per poll interval: durable state is the only
			// cross-replica source for cancellation and expiry.
			pollCtx, cancelPoll := context.WithTimeout(ctx, probePreflightPollTTL)
			current, err := s.repo.ObservePreflight(pollCtx, opID)
			cancelPoll()
			timer.Reset(every)
			if err != nil {
				if s.rootCtx.Err() == nil {
					slog.ErrorContext(s.rootCtx, "SSH preflight observation failed", "operation_id", opID)
				}
				continue
			}
			if current.Terminal() {
				// Expiry reconciled the row; terminal outcomes are immutable.
				execCancel()
				<-resultCh
				return
			}
			if current.State == domain.ProbePreflightCancelRequested {
				cancelObserved = true
				execCancel()
			}
		}
	}
}

// complete persists exactly one outcome after the inspector has been joined.
// Every write goes through the repository's owner/state/expiry fences; a
// conflict is reconciled, never forced.
func (s *ProbePreflightOperationService) complete(ctx context.Context, opID string, cancelObserved, shutdown bool, result probePreflightInspectResult) {
	outcome := s.outcome(cancelObserved, shutdown, result)
	finalCtx, cancel := s.finalWriteContext(ctx)
	defer cancel()
	_, err := s.repo.FinishPreflight(finalCtx, opID, s.ownerID, outcome)
	if err == nil {
		return
	}
	if !errors.Is(err, ports.ErrConflict) {
		// A failed finish leaves the honest nonterminal row for expiry instead
		// of fabricating a success or retrying into a false state.
		slog.ErrorContext(s.rootCtx, "SSH preflight outcome persistence failed",
			"operation_id", opID, "state", outcome.State)
		return
	}
	if outcome.State == domain.ProbePreflightCanceled {
		return
	}
	current, observeErr := s.repo.ObservePreflight(finalCtx, opID)
	if observeErr != nil {
		slog.ErrorContext(s.rootCtx, "SSH preflight reconciliation failed", "operation_id", opID)
		return
	}
	if current.State != domain.ProbePreflightCancelRequested {
		return
	}
	// Cancellation intent raced the finish after the join: the executor has
	// returned, so canceled may now be recorded under the stored fence.
	if _, finishErr := s.repo.FinishPreflight(finalCtx, opID, s.ownerID,
		domain.ProbePreflightOutcome{State: domain.ProbePreflightCanceled, ErrorCode: "canceled"}); finishErr != nil {
		slog.ErrorContext(s.rootCtx, "SSH preflight cancellation completion failed",
			"operation_id", opID)
	}
}

// outcome maps a joined inspector return to a fixed, allowlisted outcome.
// Durable cancellation intent wins over local results, success survives a
// concurrent shutdown when real facts exist, and raw errors only become fixed
// machine codes.
func (s *ProbePreflightOperationService) outcome(cancelObserved, shutdown bool, result probePreflightInspectResult) domain.ProbePreflightOutcome {
	switch {
	case cancelObserved:
		return domain.ProbePreflightOutcome{State: domain.ProbePreflightCanceled, ErrorCode: "canceled"}
	case result.err == nil && result.facts != nil:
		return domain.ProbePreflightOutcome{State: domain.ProbePreflightPassed, Result: result.facts}
	case shutdown:
		return domain.ProbePreflightOutcome{State: domain.ProbePreflightInterrupted, ErrorCode: "interrupted"}
	case result.err != nil:
		return domain.ProbePreflightOutcome{State: domain.ProbePreflightFailed, ErrorCode: probePreflightFailureCode(result.err)}
	default:
		return domain.ProbePreflightOutcome{State: domain.ProbePreflightFailed, ErrorCode: "internal"}
	}
}

// probePreflightFailureCode converts transport and compatibility errors into
// the fixed outcome allowlist. Unmapped or unexpected failures become internal.
func probePreflightFailureCode(err error) string {
	switch {
	case errors.Is(err, domain.ErrProbeSSHTrust):
		return "ssh_trust"
	case errors.Is(err, domain.ErrProbeSSHDestination):
		return "ssh_destination"
	case errors.Is(err, domain.ErrProbeSSHConnect):
		return "ssh_connect"
	case errors.Is(err, domain.ErrProbeSSHDiscovery):
		return "ssh_discovery"
	case errors.Is(err, domain.ErrProbeHostPlatform):
		return "unsupported_platform"
	case errors.Is(err, domain.ErrProbeHostPrivilege):
		return "missing_privilege"
	case errors.Is(err, domain.ErrProbeHostRuntime):
		return "missing_runtime"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "internal"
	}
}
