package services

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Coordinator bounds. Every metadata call gets a bounded five-second context;
// watcher reads get two seconds. The watcher polls storage at most once per
// second in production. None of these are configurable globals: the
// constructor fixes them and tests may only shorten their own instance.
const (
	installRunnerMetadataTimeout  = 5 * time.Second
	installRunnerWatchReadTimeout = 2 * time.Second
	installRunnerWatchPoll        = time.Second
)

// installRunnerStop classifies why the one-shot sequence is ending.
type installRunnerStop uint8

const (
	// installRunnerStopNone means the sequence may continue.
	installRunnerStopNone installRunnerStop = iota
	// installRunnerStopCancel is durable cancel_requested intent observed.
	installRunnerStopCancel
	// installRunnerStopNotStarted is a truthful joined not_started report.
	installRunnerStopNotStarted
	// installRunnerStopInterrupt is caller shutdown, caller deadline or the
	// process-monotonic run bound: owner interruption, never user cancel.
	installRunnerStopInterrupt
	// installRunnerStopObserve is unavailable or corrupt storage observation.
	installRunnerStopObserve
	// installRunnerStopForeign is a terminal/expired state or a changed
	// identity, owner or attempt: no write may follow such an observation.
	installRunnerStopForeign
)

// installRunnerSignal is the watcher's first-wins stop notification.
type installRunnerSignal struct {
	stop installRunnerStop
	err  error
}

// ProbeInstallRunner is the one-shot coordinator that connects the durable
// installation journal to a synchronous, joined executor port. It performs no
// Create, takeover, resume, renewal, admission retry or automatic redispatch:
// one invocation owns one sequence, from an acknowledged Begin to its honest
// persisted end. There is no concrete remote executor, no installer endpoint,
// no installed/success state and no remote fencing or cleanup authority here.
//
// Run returns a non-nil error only for the ends it could not complete and
// persist: invalid input, lost or mismatched authority, unusable executor
// reports, owner interruption and cold expiry. The three ends it does complete
// and persist return nil error alongside their receipt: nonterminal
// resources_ready (every resource joined applied), terminal canceled (durable
// cancellation intent with no remaining uncertainty) and terminal
// failed/not_started (the current action never invoked a mutation; earlier
// applied effects remain recorded). The returned
// operation always owns its slices; errors are fixed domain/port/context
// sentinels and never carry executor or storage diagnostics.
type ProbeInstallRunner struct {
	journal  *ProbeInstallJournalService
	executor ports.ProbeInstallResourceExecutor

	// pollInterval and runBound are per-instance test seams. The constructor
	// fixes both to the production values (installRunnerWatchPoll and
	// domain.ProbeInstallLifetime); nothing outside this package's tests may
	// shorten them, and there is no environment or global configuration.
	pollInterval time.Duration
	runBound     time.Duration
}

// NewProbeInstallRunner binds the journal facade and the executor port. The
// runner owns no state and starts no work until Run is called.
func NewProbeInstallRunner(journal *ProbeInstallJournalService, executor ports.ProbeInstallResourceExecutor) (*ProbeInstallRunner, error) {
	if journal == nil || executor == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeInstallRunner{
		journal:      journal,
		executor:     executor,
		pollInterval: installRunnerWatchPoll,
		runBound:     domain.ProbeInstallLifetime,
	}, nil
}

// Run executes exactly one canonical resource sequence for the expected
// identity under ownerID. attempts carries exactly one canonical unique
// attempt UUID per canonical resource, in layout order, and is copied before
// any I/O; IDs are supplied by the admitting composition root. The sequence
// observes the immutable identity/owner before Begin, begins exactly once,
// reserves each resource exactly once, re-observes before every invocation,
// persists every joined report on its own bounded context, and only then
// reserves dependent work. See the type comment for the return contract.
func (r *ProbeInstallRunner) Run(ctx context.Context, expected domain.ProbeInstallIdentity, ownerID string, attempts []string) (*domain.ProbeInstallOperation, error) {
	if r == nil || r.journal == nil || r.executor == nil {
		return nil, domain.ErrInternal
	}
	if ctx == nil {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !expected.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, domain.ErrValidation
	}
	specs := expected.Resources()
	// Defensive copy before I/O: later caller mutation of the slice cannot
	// reach the sequence.
	plan := append([]string(nil), attempts...)
	if len(plan) != len(specs) {
		return nil, domain.ErrValidation
	}
	seen := make(map[string]bool, len(plan))
	for _, attempt := range plan {
		if !domain.ValidProbeOperationID(attempt) || seen[attempt] {
			return nil, domain.ErrValidation
		}
		seen[attempt] = true
	}

	// The process-monotonic run bound limits the whole sequence; the caller's
	// shorter cancellation or deadline is honored because it is the parent.
	// Database authority is judged only by journal/DB state, never by
	// comparing host time to stored timestamps.
	runCtx, runCancel := context.WithTimeout(ctx, r.runBound)
	defer runCancel()

	observed, err := r.observe(runCtx, expected.OperationID)
	if err != nil {
		return nil, err
	}
	if observed.Identity != expected || observed.OwnerID != ownerID {
		// A foreign receipt is never surfaced or written to.
		return nil, ports.ErrConflict
	}
	if observed.State != domain.ProbeInstallQueued {
		return nil, ports.ErrConflict
	}

	begun, err := r.begin(runCtx, expected.OperationID, ownerID)
	if err != nil {
		// A failed or lost Begin acknowledgement never dispatches, finishes
		// or interrupts somebody else's potentially winning run; cold expiry
		// remains honest.
		return nil, err
	}
	if begun.Identity != expected || begun.OwnerID != ownerID || begun.State != domain.ProbeInstallRunning {
		return nil, domain.ErrInternal
	}
	last := begun

	watchCtx, watchCancel := context.WithCancel(context.WithoutCancel(runCtx))
	defer watchCancel()
	actionCtx, actionCancel := context.WithCancel(runCtx)
	defer actionCancel()
	signals := make(chan installRunnerSignal, 1)
	watchDone := make(chan struct{})
	go r.watch(watchCtx, expected, ownerID, expected.OperationID, actionCancel, signals, watchDone)

	joined := false
	join := func() {
		if joined {
			return
		}
		joined = true
		watchCancel()
		actionCancel()
		// The watcher is joined before any terminal outcome write or return.
		<-watchDone
	}
	defer join()

	var sig installRunnerSignal
	peek := func() installRunnerSignal {
		if sig.stop == installRunnerStopNone {
			select {
			case got := <-signals:
				sig = got
			default:
			}
		}
		return sig
	}
	stopCheck := func() installRunnerSignal {
		if got := peek(); got.stop != installRunnerStopNone {
			return got
		}
		if err := runCtx.Err(); err != nil {
			return installRunnerSignal{stop: installRunnerStopInterrupt, err: err}
		}
		return installRunnerSignal{}
	}

	for i := range specs {
		spec := specs[i]

		if got := stopCheck(); got.stop != installRunnerStopNone {
			if got.stop == installRunnerStopForeign {
				join()
				return last, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", got.stop, got.err)
		}

		reserved, created, err := r.reserve(runCtx, expected.OperationID, ownerID, spec.Kind, plan[i])
		if err != nil {
			// A lost or failed reservation acknowledgement is no grant: never
			// invoke and never continue from history. Any committed intent is
			// left for cold reconciliation.
			join()
			return last, err
		}
		if !created {
			// A historical receipt never authorizes another invocation and is
			// never acted on, not even to resolve it.
			join()
			return last, ports.ErrConflict
		}
		last = reserved
		if reserved.Identity != expected || reserved.OwnerID != ownerID {
			join()
			return nil, ports.ErrConflict
		}
		resource, ok := installFindResource(reserved, spec.Kind)
		if !ok || resource.Spec != spec || resource.AttemptID != plan[i] ||
			resource.State != domain.ProbeInstallResourceInFlight {
			join()
			return last, domain.ErrInternal
		}

		// Observational defense before every invocation: known cancellation,
		// terminal/expired state, a changed identity/owner/attempt or
		// unavailable storage stops dispatch. This is not a remote
		// transactional fence; a future adapter still owns the last remote
		// mutation guard and no DB transaction spans external I/O.
		current, err := r.observe(runCtx, expected.OperationID)
		if err != nil {
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], installRunnerStopObserve, err)
		}
		if current.Identity != expected || current.OwnerID != ownerID {
			join()
			return nil, ports.ErrConflict
		}
		switch current.State {
		case domain.ProbeInstallCancelRequested:
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], installRunnerStopCancel, nil)
		case domain.ProbeInstallRunning:
		default:
			// Terminal or expired state: never overwrite cold deadline_expired
			// history and never continue over somebody's end state.
			join()
			return current, ports.ErrConflict
		}
		observedResource, ok := installFindResource(current, spec.Kind)
		if !ok || observedResource.AttemptID != plan[i] ||
			observedResource.State != domain.ProbeInstallResourceInFlight {
			join()
			return current, ports.ErrConflict
		}
		if got := stopCheck(); got.stop != installRunnerStopNone {
			if got.stop == installRunnerStopForeign {
				join()
				return last, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], got.stop, got.err)
		}

		// Synchronous invocation on the bounded cancelable action context with
		// a defensive copy of the acknowledged reservation receipt. The port
		// returns only after all work for this invocation has joined.
		outcome, execErr := r.executor.ApplyResource(actionCtx, *installCopyOperation(reserved), spec.Kind)
		report := outcome
		if execErr != nil || !outcome.Valid() {
			// A nonnil error, an invalid or empty report and any ambiguous
			// outcome are unknown: never silently not_started or applied, and
			// never let executor errors or remote output escape.
			report = domain.ProbeInstallResourceOutcome{
				State:     domain.ProbeInstallResourceUnknown,
				ErrorCode: "outcome_unknown",
			}
		}

		// Persist the joined report on its own independent bounded context so
		// a definite applied report survives a racing caller cancellation. No
		// dependent work is ever reserved before this acknowledgement.
		recorded, err := r.recordDetached(runCtx, expected.OperationID, ownerID, spec.Kind, plan[i], report)
		if err != nil {
			// A failed or lost report acknowledgement stops the sequence
			// without redispatch and without further writes; the persisted
			// held intent is left for cold reconciliation.
			join()
			return last, err
		}
		last = recorded
		if recorded.Identity != expected || recorded.OwnerID != ownerID {
			join()
			return nil, ports.ErrConflict
		}
		switch {
		case recorded.State == domain.ProbeInstallResourcesReady:
			// The last joined effect is still truthful history, but readiness
			// must not bypass a known owner loss or watcher revocation merely
			// because there is no next loop iteration to check it.
			if got := stopCheck(); got.stop != installRunnerStopNone {
				if got.stop == installRunnerStopForeign {
					join()
					return recorded, got.err
				}
				return r.closeOut(runCtx, join, expected, ownerID, recorded, "", "", got.stop, got.err)
			}
			// Readiness is nonterminal, never installed/success. No executor
			// or watcher remains running when it returns.
			join()
			return recorded, nil
		case recorded.State == domain.ProbeInstallInterrupted:
			// The unknown report atomically interrupted the graph. Never clear
			// the uncertainty and never finish over it.
			join()
			return recorded, domain.ErrInternal
		case recorded.State == domain.ProbeInstallCancelRequested:
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", installRunnerStopCancel, nil)
		case report.State == domain.ProbeInstallResourceNotStarted:
			// A truthful joined not_started report means no dependent may ever
			// start: the sequence ends here and the owner closes it honestly.
			got := stopCheck()
			if got.stop == installRunnerStopForeign {
				join()
				return last, got.err
			}
			reason, cause := installRunnerStopNotStarted, error(nil)
			if got.stop != installRunnerStopNone {
				reason, cause = got.stop, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", reason, cause)
		default:
			// Applied and acknowledged: dependent work may now be reserved.
			continue
		}
	}

	// Unreachable: the journal moves the last committed applied report to
	// resources_ready. Never fabricate an end state for a malformed flow.
	join()
	if last.State == domain.ProbeInstallResourcesReady {
		return last, nil
	}
	return last, domain.ErrInternal
}

// closeOut joins the watcher, resolves an acknowledged but never-invoked grant
// with a truthful not_started report, and records the honest owner outcome on
// independent bounded contexts. The first failed write stops everything and
// leaves persisted held intent for cold reconciliation; nothing is fabricated
// locally and no durable uncertainty is ever cleared.
func (r *ProbeInstallRunner) closeOut(
	runCtx context.Context,
	join func(),
	expected domain.ProbeInstallIdentity,
	ownerID string,
	last *domain.ProbeInstallOperation,
	heldKind, heldAttempt string,
	reason installRunnerStop,
	cause error,
) (*domain.ProbeInstallOperation, error) {
	join()
	if heldKind != "" {
		// The coordinator knows it never called the port, so not_started is
		// truthful history rather than invented uncertainty.
		recorded, err := r.recordDetached(runCtx, expected.OperationID, ownerID, heldKind, heldAttempt,
			domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"})
		if err != nil {
			return last, err
		}
		if recorded.Identity != expected || recorded.OwnerID != ownerID {
			return nil, ports.ErrConflict
		}
		last = recorded
		if last.State == domain.ProbeInstallInterrupted {
			return last, domain.ErrInternal
		}
	}

	if last.State == domain.ProbeInstallCancelRequested || reason == installRunnerStopCancel {
		// Durable cancellation intent precedes terminal canceled, and with no
		// remaining uncertainty the joined owner may close the run canceled.
		finished, err := r.finishDetached(runCtx, expected, ownerID,
			domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"})
		if err != nil {
			return last, err
		}
		return finished, nil
	}

	interrupted := reason == installRunnerStopInterrupt || reason == installRunnerStopObserve
	if err := runCtx.Err(); err != nil {
		// Caller shutdown or deadline is owner interruption and its context
		// sentinel is the honest cause, never a fabricated user cancellation.
		interrupted = true
		cause = err
	}
	if reason == installRunnerStopNotStarted && !interrupted {
		finished, err := r.finishDetached(runCtx, expected, ownerID,
			domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "not_started"})
		if err != nil {
			return last, err
		}
		return finished, nil
	}
	finished, err := r.finishDetached(runCtx, expected, ownerID,
		domain.ProbeInstallOutcome{State: domain.ProbeInstallInterrupted, ErrorCode: "owner_interrupted"})
	if err != nil {
		return last, err
	}
	if cause == nil {
		cause = domain.ErrInternal
	}
	return finished, cause
}

// observe reads current journal state on a bounded metadata context derived
// from the caller so revocation stops the sequence at this handoff.
func (r *ProbeInstallRunner) observe(parent context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	ctx, cancel := context.WithTimeout(parent, installRunnerMetadataTimeout)
	defer cancel()
	return r.journal.Observe(ctx, operationID)
}

// begin is the single bounded queued-to-running transition.
func (r *ProbeInstallRunner) begin(parent context.Context, operationID, ownerID string) (*domain.ProbeInstallOperation, error) {
	ctx, cancel := context.WithTimeout(parent, installRunnerMetadataTimeout)
	defer cancel()
	return r.journal.Begin(ctx, operationID, ownerID)
}

// reserve binds one canonical resource to one attempt on a bounded metadata
// context derived from the caller.
func (r *ProbeInstallRunner) reserve(parent context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeInstallOperation, bool, error) {
	ctx, cancel := context.WithTimeout(parent, installRunnerMetadataTimeout)
	defer cancel()
	return r.journal.ReserveResource(ctx, operationID, ownerID, kind, attemptID)
}

// recordDetached persists one joined report on an independent bounded context,
// never an already-canceled caller, so a definite applied report survives a
// racing cancellation.
func (r *ProbeInstallRunner) recordDetached(parent context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeInstallOperation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), installRunnerMetadataTimeout)
	defer cancel()
	return r.journal.RecordResource(ctx, operationID, ownerID, kind, attemptID, outcome)
}

// finishDetached records the owner outcome on an independent bounded context.
func (r *ProbeInstallRunner) finishDetached(parent context.Context, expected domain.ProbeInstallIdentity, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeInstallOperation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), installRunnerMetadataTimeout)
	defer cancel()
	operation, err := r.journal.Finish(ctx, expected.OperationID, ownerID, outcome)
	if err != nil {
		return nil, err
	}
	// The facade binds ID/owner/outcome; the coordinator additionally binds
	// the whole expected immutable request at this final acknowledgement.
	if operation.Identity != expected || operation.OwnerID != ownerID {
		return nil, ports.ErrConflict
	}
	return operation, nil
}

// watch is the per-run storage observer started after an acknowledged Begin.
// It polls at most once per second with two-second read contexts and cancels
// the in-flight action on durable cancel_requested intent, terminal or expired
// state, identity/owner changes or an unavailable/corrupt observation. It
// exits before Run writes any terminal outcome or returns; it never writes.
func (r *ProbeInstallRunner) watch(
	ctx context.Context,
	expected domain.ProbeInstallIdentity,
	ownerID, operationID string,
	actionCancel context.CancelFunc,
	signals chan<- installRunnerSignal,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installRunnerWatchReadTimeout)
		operation, err := r.journal.Observe(readCtx, operationID)
		cancel()
		if ctx.Err() != nil {
			// Normal shutdown after the invocation joined: never record a
			// spurious stop for our own teardown.
			return
		}
		signal := installRunnerSignal{}
		switch {
		case err != nil:
			signal = installRunnerSignal{stop: installRunnerStopObserve, err: err}
		case operation.Identity != expected || operation.OwnerID != ownerID:
			signal = installRunnerSignal{stop: installRunnerStopForeign, err: ports.ErrConflict}
		case operation.State == domain.ProbeInstallCancelRequested:
			signal = installRunnerSignal{stop: installRunnerStopCancel}
		case operation.Terminal():
			signal = installRunnerSignal{stop: installRunnerStopForeign, err: ports.ErrConflict}
		default:
			continue
		}
		select {
		case signals <- signal:
		default:
		}
		actionCancel()
		return
	}
}
