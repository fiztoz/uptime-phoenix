package services

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeWatchdogTimer evaluates monotonic elapsed time. A single runtime owner
// must serialize calls and feed only generation-validated application health.
// It performs no I/O: an incident/outbox transaction must commit each proposal.
// Recreate the timer after process restart, retaining the durable armed/incident
// state but never reusing serialized wall time as a monotonic clock.
type ProbeWatchdogTimer struct {
	timing        domain.ProbeWatchdogTiming
	armed         bool
	lastEvent     time.Duration
	baseline      time.Duration
	checkpointAge time.Duration
	lastHealthy   time.Duration
	healthySince  time.Duration
	haveHealthy   bool
	latestHealthy bool
	haveStreak    bool
	pendingLoss   bool
}

// NewProbeWatchdogTimer starts a conservative grace period for a previously
// activated process. A new installation remains unarmed until Arm follows commit.
func NewProbeWatchdogTimer(timing domain.ProbeWatchdogTiming, elapsed time.Duration, armed bool) (*ProbeWatchdogTimer, error) {
	return RestoreProbeWatchdogTimer(timing, elapsed, domain.ProbeWatchdogCheckpoint{Armed: armed})
}

// RestoreProbeWatchdogTimer resumes measured loss duration without counting an
// uncertain wall-clock gap between owners. Incident identity is restored by the
// caller; a new owner never inherits a partly completed healthy recovery streak.
func RestoreProbeWatchdogTimer(timing domain.ProbeWatchdogTiming, elapsed time.Duration, checkpoint domain.ProbeWatchdogCheckpoint) (*ProbeWatchdogTimer, error) {
	if elapsed < 0 || timing.SuspectAfter <= 0 || timing.LostAfter <= timing.SuspectAfter || timing.RecoverAfter <= 0 || checkpoint.LossElapsed < 0 || !checkpoint.Armed && (checkpoint.LossElapsed != 0 || checkpoint.PendingLoss) {
		return nil, domain.ErrValidation
	}
	return &ProbeWatchdogTimer{timing: timing, armed: checkpoint.Armed, lastEvent: elapsed, baseline: elapsed, checkpointAge: min(checkpoint.LossElapsed, timing.LostAfter), pendingLoss: checkpoint.PendingLoss}, nil
}

// Checkpoint captures only measured duration and pending loss. A repository must
// atomically persist it with any corresponding incident/outbox transition under
// current watchdog ownership; persisting this value alone does not complete work.
func (w *ProbeWatchdogTimer) Checkpoint(elapsed time.Duration) (domain.ProbeWatchdogCheckpoint, error) {
	if err := w.advance(elapsed); err != nil {
		return domain.ProbeWatchdogCheckpoint{}, err
	}
	if !w.armed {
		return domain.ProbeWatchdogCheckpoint{}, nil
	}
	w.latchLoss(elapsed)
	return domain.ProbeWatchdogCheckpoint{Armed: true, LossElapsed: w.age(elapsed), PendingLoss: w.pendingLoss}, nil
}

// Arm follows durable first configuration application. Repeated config receipts
// do not reset the loss deadline or restart recovery stabilization.
func (w *ProbeWatchdogTimer) Arm(elapsed time.Duration) error {
	if err := w.advance(elapsed); err != nil {
		return err
	}
	if !w.armed {
		w.armed = true
		w.baseline = elapsed
	}
	return nil
}

// Health records one validated application-health sample. Socket traffic and
// handshake success must never call this method. Degraded samples cannot renew
// the loss deadline; a gap of at least SuspectAfter breaks recovery continuity.
func (w *ProbeWatchdogTimer) Health(elapsed time.Duration, healthy bool) error {
	if err := w.advance(elapsed); err != nil {
		return err
	}
	if !w.armed {
		return nil
	}
	w.latchLoss(elapsed)
	if !healthy {
		w.latestHealthy = false
		w.haveStreak = false
		return nil
	}
	if !w.latestHealthy || !w.haveStreak || w.age(elapsed) >= w.timing.SuspectAfter {
		w.healthySince = elapsed
		w.haveStreak = true
	}
	w.lastHealthy = elapsed
	w.haveHealthy = true
	w.latestHealthy = true
	return nil
}

// Disconnect immediately breaks a recovery streak while preserving the most
// recent application-health deadline. Reconnect alone supplies no health proof.
func (w *ProbeWatchdogTimer) Disconnect(elapsed time.Duration) error { return w.Health(elapsed, false) }

// Evaluate proposes lifecycle work against the currently persisted incident.
// Recovery requires healthy samples themselves to span RecoverAfter: advancing
// the clock after one healthy sample cannot manufacture continuous health.
func (w *ProbeWatchdogTimer) Evaluate(elapsed time.Duration, incidentOpen bool) (domain.ProbeWatchdogEvaluation, error) {
	if !w.armed && incidentOpen {
		return domain.ProbeWatchdogEvaluation{}, domain.ErrValidation
	}
	if err := w.advance(elapsed); err != nil {
		return domain.ProbeWatchdogEvaluation{}, err
	}
	if !w.armed {
		return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogUnarmed}, nil
	}
	w.latchLoss(elapsed)
	healthy := w.haveHealthy && w.latestHealthy && w.age(elapsed) < w.timing.SuspectAfter
	if incidentOpen {
		// The durable incident accounts for this loss; retries cannot open another.
		w.pendingLoss = false
		if healthy && w.haveStreak {
			if w.lastHealthy-w.healthySince >= w.timing.RecoverAfter {
				return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogHealthy, Action: domain.ProbeWatchdogResolve}, nil
			}
			return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogRecovering}, nil
		}
		return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogLost}, nil
	}
	if w.pendingLoss {
		return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogLost, Action: domain.ProbeWatchdogOpen}, nil
	}
	if healthy {
		return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogHealthy}, nil
	}
	if w.age(elapsed) >= w.timing.SuspectAfter || w.haveHealthy && !w.latestHealthy {
		return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogSuspect}, nil
	}
	return domain.ProbeWatchdogEvaluation{Status: domain.ProbeWatchdogStarting}, nil
}

func (w *ProbeWatchdogTimer) advance(elapsed time.Duration) error {
	if elapsed < 0 || elapsed < w.lastEvent {
		return domain.ErrValidation
	}
	w.lastEvent = elapsed
	return nil
}
func (w *ProbeWatchdogTimer) age(elapsed time.Duration) time.Duration {
	if w.haveHealthy {
		return min(elapsed-w.lastHealthy, w.timing.LostAfter)
	}
	age := elapsed - w.baseline
	if age >= w.timing.LostAfter-w.checkpointAge {
		return w.timing.LostAfter
	}
	return age + w.checkpointAge
}
func (w *ProbeWatchdogTimer) latchLoss(elapsed time.Duration) {
	if w.age(elapsed) >= w.timing.LostAfter {
		w.pendingLoss = true
	}
}
