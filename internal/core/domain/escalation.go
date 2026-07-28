package domain

import "time"

// Escalation state machine for a single alert's progress through a policy.
//
// pending   — more steps remain; next_run_at says when the next one is due.
// done      — every step has been sent; the alert may still be firing.
// canceled — acknowledged or resolved; no further step will ever be sent.
const (
	EscalationStatePending  = "pending"
	EscalationStateDone     = "done"
	EscalationStateCanceled = "canceled"
)

// EscalationPolicy is an ordered notification ladder that runs while an alert
// stays firing. See docs/F2.3-ESCALATION-CONTRACTS.md for the two contracts
// that govern it: precedence (monitor beats nearest ancestor group) and step
// zero (the initial DOWN notification is owned by the dispatcher, never by a
// policy).
//
// Enabled = false means "assigned but inert": the policy still stops the
// precedence walk, it simply escalates nothing. It deliberately does NOT fall
// through to the parent group — silently paging a different set of humans is
// worse than paging nobody.
type EscalationPolicy struct {
	ID          int64
	UserID      int64
	Name        string
	Description string
	Enabled     bool

	// Steps are ordered by StepOrder ascending. A policy with no steps is legal
	// and inert, the same as a disabled one.
	Steps []EscalationStep

	CreatedAt time.Time
	UpdatedAt time.Time
}

// EscalationStep is one rung of the ladder.
//
// WaitMinutes is the delay after the PREVIOUS step — for step 1, after the
// initial DOWN notification at the alert's FiredAt. It is cumulative, not an
// absolute offset from FiredAt: "wait 5 minutes, then wait 10 more" is what an
// operator means when they write 5 and 10.
type EscalationStep struct {
	ID          int64
	PolicyID    int64
	StepOrder   int
	WaitMinutes int

	// NotificationIDs are the channels this step pages. Duplicates within one
	// step are collapsed by the link table's UNIQUE(step_id, notification_id).
	NotificationIDs []int64
}

// AlertEscalation is the persisted progress of one alert through one policy.
// It is the reason escalation survives a restart: NextRunAt is the scheduling
// clock and nothing is held in memory.
//
// LeaseOwner/LeaseUntil implement the compare-and-set claim that stops two
// sharded workers sending the same step twice.
type AlertEscalation struct {
	ID        int64
	AlertID   int64
	MonitorID int64
	PolicyID  int64

	// NextStep is the StepOrder of the step that has NOT yet been sent.
	// Steps are 1-based; step 0 is the dispatcher's initial notification and is
	// never owned by a policy.
	NextStep  int
	NextRunAt time.Time

	Status string // pending | done | canceled

	LeaseOwner *string
	LeaseUntil *time.Time

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsPending reports whether more steps remain to be sent.
func (e *AlertEscalation) IsPending() bool {
	return e != nil && e.Status == EscalationStatePending
}

// ValidEscalationState reports whether s is a state this build understands.
func ValidEscalationState(s string) bool {
	switch s {
	case EscalationStatePending, EscalationStateDone, EscalationStateCanceled:
		return true
	default:
		return false
	}
}
