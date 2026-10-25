package services

import (
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Coordinator bounds. Every metadata call gets a bounded five-second context;
// watcher reads get two seconds. The watcher polls storage at most once per
// second in production. None of these are configurable globals: the
// constructor fixes them and tests may only shorten their own instance.
const (
	probeActivationRunnerMetadataTimeout  = 5 * time.Second
	probeActivationRunnerWatchReadTimeout = 2 * time.Second
	probeActivationRunnerWatchPoll        = time.Second
)

// probeActivationRunnerStop classifies why the one-shot sequence is ending.
type probeActivationRunnerStop uint8

const (
	// probeActivationStopNone means the sequence may continue.
	probeActivationStopNone probeActivationRunnerStop = iota
	// probeActivationStopCancel is durable cancel_requested intent observed.
	probeActivationStopCancel
	// probeActivationStopNotStarted is a truthful joined not_started report.
	probeActivationStopNotStarted
	// probeActivationStopInterrupt is caller shutdown, caller deadline or the
	// process-monotonic run bound: owner interruption, never user cancel.
	probeActivationStopInterrupt
	// probeActivationStopObserve is unavailable or corrupt storage observation.
	probeActivationStopObserve
	// probeActivationStopForeign is a terminal/expired state or a changed
	// identity or owner: no write may follow such an observation.
	probeActivationStopForeign
)

// probeActivationRunnerSignal is the watcher's first-wins stop notification.
type probeActivationRunnerSignal struct {
	stop probeActivationRunnerStop
	err  error
}

// ProbeActivationRunner is the one-shot coordinator that connects the durable
// activation journal to a synchronous, joined step executor port. It mirrors
// ProbeInstallRunner: it performs no Create, takeover, resume, renewal,
// admission retry or automatic redispatch, and one invocation owns one
// sequence, from an acknowledged Begin to its honest persisted end. It holds
// no secret: the one-time enrollment token never reaches the runner, only the
// executor that owns the init and enrollment steps ever sees it.
//
// Run returns a non-nil error only for the ends it could not complete and
// persist: invalid input, lost or mismatched authority, unusable executor
// reports, owner interruption and cold expiry. The ends it does complete and
// persist return nil error alongside their receipt: terminal active (every
// step joined applied), terminal canceled (durable cancellation intent with no
// remaining uncertainty) and terminal failed/not_started (the current step
// never invoked a mutation; earlier applied effects remain recorded). The
// returned activation always owns its slices; errors are fixed domain, port
// or context sentinels and never carry executor or storage diagnostics.
//
// The runner also performs the receipt validation a journal facade would: every
// receipt it acts on must be non-nil, valid and bound to the expected operation,
// identity and owner, and executor facts are recorded only where the journal
// contract allows them.
type ProbeActivationRunner struct {
	repo     ports.ProbeActivationRepository
	steps    ports.ProbeActivationStepJournal
	executor ports.ProbeActivationStepExecutor

	// pollInterval and runBound are per-instance test seams. The constructor
	// fixes both to the production values (probeActivationRunnerWatchPoll and
	// domain.ProbeActivationLifetime); nothing outside this package's tests may
	// shorten them, and there is no environment or global configuration.
	pollInterval time.Duration
	runBound     time.Duration
}

// NewProbeActivationRunner binds the activation journal ports and the step
// executor. The runner owns no state and starts no work until Run is called.
func NewProbeActivationRunner(repo ports.ProbeActivationRepository, steps ports.ProbeActivationStepJournal, executor ports.ProbeActivationStepExecutor) (*ProbeActivationRunner, error) {
	if repo == nil || steps == nil || executor == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeActivationRunner{
		repo:         repo,
		steps:        steps,
		executor:     executor,
		pollInterval: probeActivationRunnerWatchPoll,
		runBound:     domain.ProbeActivationLifetime,
	}, nil
}

// Run executes exactly one canonical activation step sequence for the expected
// identity under ownerID. attempts carries exactly one canonical unique attempt
// UUID per canonical step, in layout order, and is copied before any I/O; IDs
// are supplied by the admitting composition root. The sequence observes the
// immutable identity/owner before Begin, begins exactly once, reserves each
// step exactly once, re-observes before every invocation, persists every joined
// report on its own bounded context, and only then reserves dependent work. See
// the type comment for the return contract.
func (r *ProbeActivationRunner) Run(ctx context.Context, expected domain.ProbeActivationIdentity, ownerID string, attempts []string) (*domain.ProbeActivation, error) {
	if r == nil || r.repo == nil || r.steps == nil || r.executor == nil {
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
	specs := expected.Steps()
	operationID := expected.Install.OperationID
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

	observed, err := r.observe(runCtx, operationID)
	if err != nil {
		return nil, err
	}
	if observed.Identity != expected || observed.OwnerID != ownerID {
		// A foreign receipt is never surfaced or written to.
		return nil, ports.ErrConflict
	}
	if observed.State != domain.ProbeActivationQueued {
		return nil, ports.ErrConflict
	}

	begun, err := r.begin(runCtx, operationID, ownerID)
	if err != nil {
		// A failed or lost Begin acknowledgement never dispatches, finishes
		// or interrupts somebody else's potentially winning run; cold expiry
		// remains honest.
		return nil, err
	}
	if begun.Identity != expected || begun.OwnerID != ownerID || begun.State != domain.ProbeActivationRunning {
		return nil, domain.ErrInternal
	}
	last := begun

	watchCtx, watchCancel := context.WithCancel(context.WithoutCancel(runCtx))
	defer watchCancel()
	actionCtx, actionCancel := context.WithCancel(runCtx)
	defer actionCancel()
	signals := make(chan probeActivationRunnerSignal, 1)
	watchDone := make(chan struct{})
	go r.watch(watchCtx, expected, ownerID, operationID, actionCancel, signals, watchDone)

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

	var sig probeActivationRunnerSignal
	peek := func() probeActivationRunnerSignal {
		if sig.stop == probeActivationStopNone {
			select {
			case got := <-signals:
				sig = got
			default:
			}
		}
		return sig
	}
	stopCheck := func() probeActivationRunnerSignal {
		if got := peek(); got.stop != probeActivationStopNone {
			return got
		}
		if err := runCtx.Err(); err != nil {
			return probeActivationRunnerSignal{stop: probeActivationStopInterrupt, err: err}
		}
		return probeActivationRunnerSignal{}
	}

	for i := range specs {
		spec := specs[i]

		if got := stopCheck(); got.stop != probeActivationStopNone {
			if got.stop == probeActivationStopForeign {
				join()
				return last, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", got.stop, got.err)
		}

		reserved, created, err := r.reserve(runCtx, operationID, ownerID, spec.Kind, plan[i])
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
		step, ok := probeActivationRunnerFindStep(reserved, spec.Kind)
		if !ok || step.Spec != spec || step.AttemptID != plan[i] ||
			step.State != domain.ProbeInstallResourceInFlight {
			join()
			return last, domain.ErrInternal
		}

		// Observational defense before every invocation: known cancellation,
		// terminal/expired state, a changed identity/owner/attempt or
		// unavailable storage stops dispatch. This is not a remote
		// transactional fence; the authorized host helper and the admission
		// table still own the last remote mutation guard and no DB transaction
		// spans external I/O.
		current, err := r.observe(runCtx, operationID)
		if err != nil {
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], probeActivationStopObserve, err)
		}
		if current.Identity != expected || current.OwnerID != ownerID {
			join()
			return nil, ports.ErrConflict
		}
		switch current.State {
		case domain.ProbeActivationCancelRequested:
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], probeActivationStopCancel, nil)
		case domain.ProbeActivationRunning:
		default:
			// Terminal or expired state: never overwrite cold deadline_expired
			// history and never continue over somebody's end state.
			join()
			return current, ports.ErrConflict
		}
		observedStep, ok := probeActivationRunnerFindStep(current, spec.Kind)
		if !ok || observedStep.AttemptID != plan[i] ||
			observedStep.State != domain.ProbeInstallResourceInFlight {
			join()
			return current, ports.ErrConflict
		}
		if got := stopCheck(); got.stop != probeActivationStopNone {
			if got.stop == probeActivationStopForeign {
				join()
				return last, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, spec.Kind, plan[i], got.stop, got.err)
		}

		// Synchronous invocation on the bounded cancelable action context with
		// a defensive copy of the acknowledged reservation receipt. The port
		// returns only after all work for this invocation has joined.
		outcome, facts, execErr := r.executor.ApplyStep(actionCtx, *probeActivationRunnerCopy(reserved), spec.Kind)
		report, reportFacts := probeActivationRunnerReport(spec.Kind, outcome, facts, execErr)

		// Persist the joined report on its own independent bounded context so
		// a definite applied report survives a racing caller cancellation. No
		// dependent work is ever reserved before this acknowledgement.
		recorded, err := r.recordDetached(runCtx, operationID, ownerID, spec.Kind, plan[i], report, reportFacts)
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
		case recorded.State == domain.ProbeActivationActive:
			// Active is terminal success, recorded by the journal together with
			// the last applied step. There is nothing left to close out and the
			// watcher may legitimately have observed the terminal state, so no
			// stop signal can revoke it. No executor or watcher remains running
			// when it returns.
			join()
			return recorded, nil
		case recorded.State == domain.ProbeActivationInterrupted:
			// The unknown report atomically interrupted the graph. Never clear
			// the uncertainty and never finish over it.
			join()
			return recorded, domain.ErrInternal
		case recorded.State == domain.ProbeActivationCancelRequested:
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", probeActivationStopCancel, nil)
		case report.State == domain.ProbeInstallResourceNotStarted:
			// A truthful joined not_started report means no dependent may ever
			// start: the sequence ends here and the owner closes it honestly.
			got := stopCheck()
			if got.stop == probeActivationStopForeign {
				join()
				return last, got.err
			}
			reason, cause := probeActivationStopNotStarted, error(nil)
			if got.stop != probeActivationStopNone {
				reason, cause = got.stop, got.err
			}
			return r.closeOut(runCtx, join, expected, ownerID, last, "", "", reason, cause)
		default:
			// Applied and acknowledged: dependent work may now be reserved.
			continue
		}
	}

	// Unreachable: the journal moves the last committed applied report to
	// active. Never fabricate an end state for a malformed flow.
	join()
	if last.State == domain.ProbeActivationActive {
		return last, nil
	}
	return last, domain.ErrInternal
}

// probeActivationRunnerReport turns the executor's raw return into the one
// report the runner may persist. A nonnil error, an invalid or empty outcome,
// facts anywhere but on an applied probe_init, and invalid or missing facts on
// an applied probe_init are all unknown: never silently not_started or
// applied, never executor-provided facts, and never executor error text.
func probeActivationRunnerReport(kind string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts, execErr error) (domain.ProbeInstallResourceOutcome, domain.ProbeActivationFacts) {
	unknown := domain.ProbeInstallResourceOutcome{
		State:     domain.ProbeInstallResourceUnknown,
		ErrorCode: "outcome_unknown",
	}
	if execErr != nil || !outcome.Valid() {
		return unknown, domain.ProbeActivationFacts{}
	}
	if kind == domain.ProbeActivationProbeInit && outcome.State == domain.ProbeInstallResourceApplied {
		if !facts.Valid() {
			return unknown, domain.ProbeActivationFacts{}
		}
		return outcome, facts
	}
	if facts != (domain.ProbeActivationFacts{}) {
		return unknown, domain.ProbeActivationFacts{}
	}
	return outcome, domain.ProbeActivationFacts{}
}

// closeOut joins the watcher, resolves an acknowledged but never-invoked grant
// with a truthful not_started report, and records the honest owner outcome on
// independent bounded contexts. The first failed write stops everything and
// leaves persisted held intent for cold reconciliation; nothing is fabricated
// locally and no durable uncertainty is ever cleared.
func (r *ProbeActivationRunner) closeOut(
	runCtx context.Context,
	join func(),
	expected domain.ProbeActivationIdentity,
	ownerID string,
	last *domain.ProbeActivation,
	heldKind, heldAttempt string,
	reason probeActivationRunnerStop,
	cause error,
) (*domain.ProbeActivation, error) {
	join()
	if heldKind != "" {
		// The coordinator knows it never called the port, so not_started is
		// truthful history rather than invented uncertainty.
		recorded, err := r.recordDetached(runCtx, expected.Install.OperationID, ownerID, heldKind, heldAttempt,
			domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"},
			domain.ProbeActivationFacts{})
		if err != nil {
			return last, err
		}
		if recorded.Identity != expected || recorded.OwnerID != ownerID {
			return nil, ports.ErrConflict
		}
		last = recorded
		if last.State == domain.ProbeActivationInterrupted {
			return last, domain.ErrInternal
		}
	}

	if last.State == domain.ProbeActivationCancelRequested || reason == probeActivationStopCancel {
		// Durable cancellation intent precedes terminal canceled, and with no
		// remaining uncertainty the joined owner may close the run canceled.
		finished, err := r.finishDetached(runCtx, expected, ownerID,
			domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"})
		if err != nil {
			return last, err
		}
		return finished, nil
	}

	interrupted := reason == probeActivationStopInterrupt || reason == probeActivationStopObserve
	if err := runCtx.Err(); err != nil {
		// Caller shutdown or deadline is owner interruption and its context
		// sentinel is the honest cause, never a fabricated user cancellation.
		interrupted = true
		cause = err
	}
	if reason == probeActivationStopNotStarted && !interrupted {
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
func (r *ProbeActivationRunner) observe(parent context.Context, operationID string) (*domain.ProbeActivation, error) {
	ctx, cancel := context.WithTimeout(parent, probeActivationRunnerMetadataTimeout)
	defer cancel()
	return r.journalObserve(ctx, operationID)
}

// begin is the single bounded queued-to-running transition.
func (r *ProbeActivationRunner) begin(parent context.Context, operationID, ownerID string) (*domain.ProbeActivation, error) {
	ctx, cancel := context.WithTimeout(parent, probeActivationRunnerMetadataTimeout)
	defer cancel()
	return r.journalBegin(ctx, operationID, ownerID)
}

// reserve binds one canonical step to one attempt on a bounded metadata
// context derived from the caller.
func (r *ProbeActivationRunner) reserve(parent context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeActivation, bool, error) {
	ctx, cancel := context.WithTimeout(parent, probeActivationRunnerMetadataTimeout)
	defer cancel()
	return r.journalReserve(ctx, operationID, ownerID, kind, attemptID)
}

// recordDetached persists one joined report on an independent bounded context,
// never an already-canceled caller, so a definite applied report survives a
// racing cancellation.
func (r *ProbeActivationRunner) recordDetached(parent context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts) (*domain.ProbeActivation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), probeActivationRunnerMetadataTimeout)
	defer cancel()
	return r.journalRecord(ctx, operationID, ownerID, kind, attemptID, outcome, facts)
}

// finishDetached records the owner outcome on an independent bounded context.
func (r *ProbeActivationRunner) finishDetached(parent context.Context, expected domain.ProbeActivationIdentity, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeActivation, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), probeActivationRunnerMetadataTimeout)
	defer cancel()
	activation, err := r.journalFinish(ctx, expected.Install.OperationID, ownerID, outcome)
	if err != nil {
		return nil, err
	}
	// The journal checks bind ID/owner/outcome; the coordinator additionally
	// binds the whole expected immutable request at this final acknowledgement.
	if activation.Identity != expected || activation.OwnerID != ownerID {
		return nil, ports.ErrConflict
	}
	return activation, nil
}

// watch is the per-run storage observer started after an acknowledged Begin.
// It polls at most once per second with two-second read contexts and cancels
// the in-flight action on durable cancel_requested intent, terminal or expired
// state, identity/owner changes or an unavailable/corrupt observation. It
// exits before Run writes any terminal outcome or returns; it never writes.
func (r *ProbeActivationRunner) watch(
	ctx context.Context,
	expected domain.ProbeActivationIdentity,
	ownerID, operationID string,
	actionCancel context.CancelFunc,
	signals chan<- probeActivationRunnerSignal,
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
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeActivationRunnerWatchReadTimeout)
		activation, err := r.journalObserve(readCtx, operationID)
		cancel()
		if ctx.Err() != nil {
			// Normal shutdown after the invocation joined: never record a
			// spurious stop for our own teardown.
			return
		}
		var signal probeActivationRunnerSignal
		switch {
		case err != nil:
			signal = probeActivationRunnerSignal{stop: probeActivationStopObserve, err: err}
		case activation.Identity != expected || activation.OwnerID != ownerID:
			signal = probeActivationRunnerSignal{stop: probeActivationStopForeign, err: ports.ErrConflict}
		case activation.State == domain.ProbeActivationCancelRequested:
			signal = probeActivationRunnerSignal{stop: probeActivationStopCancel}
		case activation.Terminal():
			signal = probeActivationRunnerSignal{stop: probeActivationStopForeign, err: ports.ErrConflict}
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

// journalObserve reads the durable activation and its complete step graph. It
// never creates, dispatches, renews or reconciles anything on its own, and the
// receipt must be a complete valid graph for exactly the requested operation.
func (r *ProbeActivationRunner) journalObserve(ctx context.Context, operationID string) (*domain.ProbeActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) {
		return nil, domain.ErrValidation
	}
	activation, err := r.repo.ObserveActivation(ctx, operationID)
	if err != nil {
		return nil, probeActivationRunnerError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := probeActivationRunnerValidate(activation, operationID); err != nil {
		return nil, err
	}
	return probeActivationRunnerCopy(activation), nil
}

// journalBegin is the single queued-to-running transition under the stored
// owner. The returned graph must show the admitted owner running, so a stale or
// corrupt acknowledgement is never a dispatch grant.
func (r *ProbeActivationRunner) journalBegin(ctx context.Context, operationID, ownerID string) (*domain.ProbeActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, domain.ErrValidation
	}
	activation, err := r.repo.BeginActivation(ctx, operationID, ownerID)
	if err != nil {
		return nil, probeActivationRunnerError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := probeActivationRunnerValidate(activation, operationID); err != nil {
		return nil, err
	}
	if activation.OwnerID != ownerID || activation.State != domain.ProbeActivationRunning || activation.ErrorCode != "" {
		return nil, domain.ErrInternal
	}
	return probeActivationRunnerCopy(activation), nil
}

// journalFinish records exactly one owner outcome: failed, canceled or the
// owner's own interruption. Active is never an outcome; the journal reaches it
// only by recording the last applied step. The stored graph must show the
// joined owner and the exact requested terminal outcome, so a stale, repaired
// or fabricated acknowledgement is never accepted.
func (r *ProbeActivationRunner) journalFinish(ctx context.Context, operationID, ownerID string, outcome domain.ProbeInstallOutcome) (*domain.ProbeActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) || !outcome.Valid() {
		return nil, domain.ErrValidation
	}
	activation, err := r.repo.FinishActivation(ctx, operationID, ownerID, outcome)
	if err != nil {
		return nil, probeActivationRunnerError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := probeActivationRunnerValidate(activation, operationID); err != nil {
		return nil, err
	}
	if activation.OwnerID != ownerID || !activation.Terminal() ||
		activation.State != outcome.State || activation.ErrorCode != outcome.ErrorCode {
		return nil, domain.ErrInternal
	}
	return probeActivationRunnerCopy(activation), nil
}

// journalReserve binds one canonical step to one canonical attempt UUID under
// the stored owner. created=true is a grant for exactly one external invocation
// and requires the admitted owner running with that step committed in_flight.
// created=false never grants anything: exact retries, including historical
// outcomes after expiry, must not be re-executed.
func (r *ProbeActivationRunner) journalReserve(ctx context.Context, operationID, ownerID, kind, attemptID string) (*domain.ProbeActivation, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!probeActivationRunnerValidKind(kind) || !domain.ValidProbeOperationID(attemptID) {
		return nil, false, domain.ErrValidation
	}
	activation, created, err := r.steps.ReserveActivationStep(ctx, operationID, ownerID, kind, attemptID)
	if err != nil {
		return nil, false, probeActivationRunnerError(err)
	}
	if err := ctx.Err(); err != nil {
		// A canceled post-commit acknowledgement never returns a fresh grant.
		return nil, false, err
	}
	if err := probeActivationRunnerValidate(activation, operationID); err != nil {
		return nil, false, err
	}
	step, ok := probeActivationRunnerFindStep(activation, kind)
	if !ok || activation.OwnerID != ownerID || step.AttemptID != attemptID {
		return nil, false, domain.ErrInternal
	}
	if created && (activation.State != domain.ProbeActivationRunning ||
		step.State != domain.ProbeInstallResourceInFlight) {
		// Only an exact in-flight intent on the running, admitted owner is a
		// fresh action grant.
		return nil, false, domain.ErrInternal
	}
	// created=false is returned exactly as received and never authorizes
	// another invocation, whatever the recorded state or history.
	return probeActivationRunnerCopy(activation), created, nil
}

// journalRecord reports one joined step action for an exact stored attempt
// under the stored owner. Facts are accepted only with an applied probe_init
// and must come back stored with it; every other report carries zero facts.
// Applied/not_started/unknown reports are immutable history: the stored
// outcome must match the request exactly, and a late or altered
// acknowledgement is rejected rather than repaired.
func (r *ProbeActivationRunner) journalRecord(ctx context.Context, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome, facts domain.ProbeActivationFacts) (*domain.ProbeActivation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	withFacts := kind == domain.ProbeActivationProbeInit && outcome.State == domain.ProbeInstallResourceApplied
	if !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) ||
		!probeActivationRunnerValidKind(kind) || !domain.ValidProbeOperationID(attemptID) || !outcome.Valid() ||
		(withFacts && !facts.Valid()) || (!withFacts && facts != (domain.ProbeActivationFacts{})) {
		return nil, domain.ErrValidation
	}
	activation, err := r.steps.RecordActivationStep(ctx, operationID, ownerID, kind, attemptID, outcome, facts)
	if err != nil {
		return nil, probeActivationRunnerError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := probeActivationRunnerValidate(activation, operationID); err != nil {
		return nil, err
	}
	step, ok := probeActivationRunnerFindStep(activation, kind)
	if !ok || activation.OwnerID != ownerID || step.AttemptID != attemptID ||
		step.State != outcome.State || step.ErrorCode != outcome.ErrorCode {
		return nil, domain.ErrInternal
	}
	if withFacts && activation.Facts != facts {
		// The facts are stored atomically with probe_init applied and must
		// read back exactly; anything else is a corrupt acknowledgement.
		return nil, domain.ErrInternal
	}
	return probeActivationRunnerCopy(activation), nil
}

// probeActivationRunnerError maps every repository failure onto the fixed
// public sentinels. Not-found, conflict and context causes survive unchanged;
// repo corruption and unknown raw errors (which may carry private diagnostics)
// collapse to the fixed domain.ErrInternal without leaking their text.
func probeActivationRunnerError(err error) error {
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

// probeActivationRunnerValidate checks that a receipt exists, is a complete
// valid graph and is the requested operation before the runner acts on it.
func probeActivationRunnerValidate(activation *domain.ProbeActivation, operationID string) error {
	if activation == nil || !activation.Valid() || activation.Identity.Install.OperationID != operationID {
		return domain.ErrInternal
	}
	return nil
}

// probeActivationRunnerCopy returns a defensive copy whose step slice cannot
// be mutated through (or corrupted by) the caller or the repository receipt.
func probeActivationRunnerCopy(activation *domain.ProbeActivation) *domain.ProbeActivation {
	copied := *activation
	copied.Steps = append([]domain.ProbeInstallResource(nil), activation.Steps...)
	return &copied
}

// probeActivationRunnerFindStep locates the one canonical step of the given kind.
func probeActivationRunnerFindStep(activation *domain.ProbeActivation, kind string) (domain.ProbeInstallResource, bool) {
	for _, step := range activation.Steps {
		if step.Spec.Kind == kind {
			return step, true
		}
	}
	return domain.ProbeInstallResource{}, false
}

// probeActivationRunnerValidKind accepts only the four activation step kinds
// derived by ProbeActivationIdentity.Steps(); references are never
// caller-controlled.
func probeActivationRunnerValidKind(kind string) bool {
	switch kind {
	case domain.ProbeActivationProbeInit, domain.ProbeActivationRegistration,
		domain.ProbeActivationServiceStart, domain.ProbeActivationEnrollment:
		return true
	default:
		return false
	}
}
