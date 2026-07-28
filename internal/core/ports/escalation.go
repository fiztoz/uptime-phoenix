package ports

import (
	"context"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// EscalationPolicyRepository persists escalation policies together with their
// ordered steps and each step's notification channels (F2.3).
//
// A policy is loaded and written as a whole: Create and Update carry the full
// step list. Steps are meaningless apart from their policy and the UI edits
// them as one ordered form, so a per-step CRUD surface would only invite
// partial writes.
type EscalationPolicyRepository interface {
	// Create inserts the policy and its steps in one transaction, assigning
	// ids and timestamps back onto p.
	Create(ctx context.Context, p *domain.EscalationPolicy) error
	// Update rewrites the policy and REPLACES its entire step list in one
	// transaction. Steps absent from p.Steps are deleted — this is a
	// replace-set, exactly like PUT /api/status-pages/:spId/monitors, and a
	// caller sending a partial list silently drops steps. Send the whole ladder.
	Update(ctx context.Context, p *domain.EscalationPolicy) error
	// GetByID returns one policy with Steps populated in StepOrder ascending.
	GetByID(ctx context.Context, id int64) (*domain.EscalationPolicy, error)
	// List returns every policy in the install with Steps populated, ordered by
	// id. Callers must have checked the caller's capability; this performs no
	// authorization of its own.
	List(ctx context.Context) ([]*domain.EscalationPolicy, error)
	Delete(ctx context.Context, id int64) error
}

// EscalationAssignmentRepository persists which policy escalates a monitor or a
// monitor group (F2.3 contract 1).
//
// Both link tables carry a UNIQUE on the entity column, so assigning replaces
// any previous assignment rather than accumulating. That is the schema, not
// application code, enforcing "at most one policy per monitor".
type EscalationAssignmentRepository interface {
	// AssignMonitor points a monitor at a policy, replacing any prior
	// assignment. Idempotent.
	AssignMonitor(ctx context.Context, monitorID, policyID int64) error
	// UnassignMonitor removes the monitor's assignment. Removing an absent
	// assignment is not an error.
	UnassignMonitor(ctx context.Context, monitorID int64) error
	// PolicyIDForMonitor returns the monitor's directly assigned policy id, or
	// ErrNotFound when the monitor has none. It does NOT walk ancestor groups —
	// precedence is the service's job (see EscalationService.ResolvePolicy).
	PolicyIDForMonitor(ctx context.Context, monitorID int64) (int64, error)

	// AssignGroup points a monitor group at a policy, replacing any prior
	// assignment. Idempotent.
	AssignGroup(ctx context.Context, groupID, policyID int64) error
	// UnassignGroup removes the group's assignment.
	UnassignGroup(ctx context.Context, groupID int64) error
	// PolicyIDForGroup returns the group's directly assigned policy id, or
	// ErrNotFound when the group has none.
	PolicyIDForGroup(ctx context.Context, groupID int64) (int64, error)

	// ListMonitorsByPolicy returns the monitor IDs that are directly assigned
	// to this policy, ordered by monitor_id. It does NOT expand group
	// inheritance — only rows in the link table.
	ListMonitorsByPolicy(ctx context.Context, policyID int64) ([]int64, error)
	// ListGroupsByPolicy returns the group IDs that are directly assigned to
	// this policy, ordered by group_id.
	ListGroupsByPolicy(ctx context.Context, policyID int64) ([]int64, error)
}

// AlertEscalationRepository persists one alert's progress through a policy.
//
// Progress is a row, never memory: NextRunAt is the scheduling clock, so a
// worker that restarts mid-ladder resumes at NextStep instead of starting over
// or dropping the escalation entirely.
type AlertEscalationRepository interface {
	// Create starts the ladder for an alert. A UNIQUE(alert_id) conflict means
	// another worker already started it and is returned as ErrConflict — the
	// caller treats that as success, not as an error worth logging loudly.
	Create(ctx context.Context, e *domain.AlertEscalation) error
	// GetByAlertID returns the escalation for an alert, or ErrNotFound.
	GetByAlertID(ctx context.Context, alertID int64) (*domain.AlertEscalation, error)
	// ListByAlertIDs returns the escalations for many alerts, keyed by alert id.
	//
	// One query, not one per alert. The alert list page renders an escalation
	// badge per row, and a per-row lookup there is exactly the N+1 that made
	// Sprint D's event fan-out O(monitors²) — see docs/SPRINT-D-HANDOFF.md.
	// Alerts without an escalation are simply absent from the map.
	ListByAlertIDs(ctx context.Context, alertIDs []int64) (map[int64]*domain.AlertEscalation, error)

	// ClaimDue atomically leases every pending row whose NextRunAt has arrived
	// and whose lease is absent or expired, stamping claimToken as the owner,
	// then returns exactly the rows this call won.
	//
	// claimToken must be unique per call (worker id + nonce): the claim is a
	// compare-and-set and the token is what proves ownership to Advance and
	// Finish. Two workers polling the same instant cannot both win a row.
	ClaimDue(ctx context.Context, claimToken string, now, leaseUntil time.Time) ([]*domain.AlertEscalation, error)

	// Advance moves a claimed row to the next step and releases the lease. It is
	// guarded on claimToken, so a worker whose lease expired mid-send cannot
	// clobber a row another worker has since taken. ok reports whether this
	// caller still owned the row.
	Advance(ctx context.Context, id int64, claimToken string, nextStep int, nextRunAt time.Time) (ok bool, err error)

	// Finish marks a claimed row done or canceled and releases the lease, with
	// the same claimToken guard as Advance.
	Finish(ctx context.Context, id int64, claimToken, status string) (ok bool, err error)

	// CancelByAlertID cancels the alert's pending escalation regardless of any
	// held lease — an acknowledgement must always win the race against a worker
	// that is mid-step. Canceling an absent or already-finished escalation is
	// not an error. The row is kept (status='canceled'), never deleted: it is
	// the audit trail of how far the ladder got before a human stepped in.
	CancelByAlertID(ctx context.Context, alertID int64) error
}
