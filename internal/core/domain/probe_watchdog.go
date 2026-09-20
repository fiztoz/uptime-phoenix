package domain

import "time"

// ProbeWatchdogTiming is the application-health timing contract shared by both
// connection owners. Durations are measured with a process monotonic clock.
type ProbeWatchdogTiming struct {
	SuspectAfter time.Duration
	LostAfter    time.Duration
	RecoverAfter time.Duration
}

// DefaultProbeWatchdogTiming is the V1 45/90/30-second connection policy.
func DefaultProbeWatchdogTiming() ProbeWatchdogTiming {
	return ProbeWatchdogTiming{SuspectAfter: 45 * time.Second, LostAfter: 90 * time.Second, RecoverAfter: 30 * time.Second}
}

// ProbeWatchdogStatus describes connection health, never monitor availability.
type ProbeWatchdogStatus string

const (
	ProbeWatchdogUnarmed    ProbeWatchdogStatus = "unarmed"
	ProbeWatchdogStarting   ProbeWatchdogStatus = "starting"
	ProbeWatchdogHealthy    ProbeWatchdogStatus = "healthy"
	ProbeWatchdogSuspect    ProbeWatchdogStatus = "suspect"
	ProbeWatchdogLost       ProbeWatchdogStatus = "lost"
	ProbeWatchdogRecovering ProbeWatchdogStatus = "recovering"
)

// ProbeWatchdogAction is a proposal for the transactional incident owner. It is
// not an acknowledgement that an incident or provider intent was persisted.
type ProbeWatchdogAction string

const (
	ProbeWatchdogNoAction ProbeWatchdogAction = ""
	ProbeWatchdogOpen     ProbeWatchdogAction = "open"
	ProbeWatchdogResolve  ProbeWatchdogAction = "resolve"
)

// ProbeWatchdogEvaluation separates live diagnostics from durable lifecycle work.
// Existing firing and acknowledged incidents both count as open until resolved.
type ProbeWatchdogEvaluation struct {
	Status ProbeWatchdogStatus
	Action ProbeWatchdogAction
}

// ProbeWatchdogCheckpoint retains measured unhealthy time across owner handoff.
// It contains a duration, never an absolute monotonic timestamp or a wall-clock
// subtraction. Recovery continuity must be re-established by the new owner.
type ProbeWatchdogCheckpoint struct {
	Armed       bool
	LossElapsed time.Duration
	PendingLoss bool
}
