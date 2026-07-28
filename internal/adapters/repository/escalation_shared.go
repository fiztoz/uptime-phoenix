package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The helpers below are shared by the SQLite and MariaDB escalation adapters.
//
// Sharing is deliberate here, and it is the opposite call from ReorderMonitors
// (AGENTS.md / handoff §4.3), where the two adapters legitimately diverge. These
// statements are portable ANSI UPDATEs with positional placeholders and no
// engine-specific syntax, so two hand-maintained copies would only create room
// for the compare-and-set semantics to drift apart on one engine — which is
// exactly the class of bug the MariaDB contract exists to catch.
//
// The genuinely engine-specific statements — the assignment upserts — stay in
// their own adapters. Errors are returned raw: each adapter maps them with its
// own translateError.

// InsertEscalationSteps writes a policy's whole step ladder, including each
// step's notification links. Callers pass a transaction; this is never a
// standalone operation.
func InsertEscalationSteps(ctx context.Context, tx bun.IDB, policyID int64, steps []domain.EscalationStep) error {
	for i := range steps {
		sm := &EscalationStepModel{
			PolicyID:    policyID,
			StepOrder:   steps[i].StepOrder,
			WaitMinutes: steps[i].WaitMinutes,
		}
		if _, err := tx.NewInsert().Model(sm).Exec(ctx); err != nil {
			return err
		}
		steps[i].ID = sm.ID
		steps[i].PolicyID = policyID

		seen := make(map[int64]struct{}, len(steps[i].NotificationIDs))
		for _, nid := range steps[i].NotificationIDs {
			if _, dup := seen[nid]; dup {
				// UNIQUE(step_id, notification_id) would reject this anyway; skip
				// it here so a duplicated channel in the form is collapsed rather
				// than failing the whole save.
				continue
			}
			seen[nid] = struct{}{}
			link := &EscalationStepNotificationModel{StepID: sm.ID, NotificationID: nid}
			if _, err := tx.NewInsert().Model(link).Exec(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}

// LoadEscalationSteps returns the steps of each requested policy, keyed by
// policy id and ordered by StepOrder ascending, with NotificationIDs populated.
//
// Two queries total, not one per step: a policy list page would otherwise be an
// N+1 the moment an install has more than a handful of ladders.
func LoadEscalationSteps(ctx context.Context, db bun.IDB, policyIDs []int64) (map[int64][]domain.EscalationStep, error) {
	out := make(map[int64][]domain.EscalationStep, len(policyIDs))
	if len(policyIDs) == 0 {
		return out, nil
	}

	var stepModels []*EscalationStepModel
	if err := db.NewSelect().Model(&stepModels).
		Where("policy_id IN (?)", bun.List(policyIDs)).
		Order("policy_id ASC", "step_order ASC", "id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	if len(stepModels) == 0 {
		return out, nil
	}

	stepIDs := make([]int64, len(stepModels))
	for i, sm := range stepModels {
		stepIDs[i] = sm.ID
	}
	var links []*EscalationStepNotificationModel
	if err := db.NewSelect().Model(&links).
		Where("step_id IN (?)", bun.List(stepIDs)).
		Order("step_id ASC", "notification_id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}
	byStep := make(map[int64][]int64, len(stepModels))
	for _, l := range links {
		byStep[l.StepID] = append(byStep[l.StepID], l.NotificationID)
	}

	for _, sm := range stepModels {
		out[sm.PolicyID] = append(out[sm.PolicyID], domain.EscalationStep{
			ID:              sm.ID,
			PolicyID:        sm.PolicyID,
			StepOrder:       sm.StepOrder,
			WaitMinutes:     sm.WaitMinutes,
			NotificationIDs: byStep[sm.ID],
		})
	}
	return out, nil
}

// ListEscalationsByAlertIDs returns the escalations for many alerts in ONE
// query, keyed by alert id.
func ListEscalationsByAlertIDs(ctx context.Context, db bun.IDB, alertIDs []int64) (map[int64]*domain.AlertEscalation, error) {
	out := make(map[int64]*domain.AlertEscalation, len(alertIDs))
	if len(alertIDs) == 0 {
		return out, nil
	}
	var models []*AlertEscalationModel
	if err := db.NewSelect().Model(&models).
		Where("alert_id IN (?)", bun.List(alertIDs)).
		Scan(ctx); err != nil {
		return nil, err
	}
	for _, m := range models {
		out[m.AlertID] = m.ToDomain()
	}
	return out, nil
}

// ClaimDueEscalations is the compare-and-set lease claim. It stamps claimToken
// on every pending row that is due and unleased (or whose lease has expired),
// then returns exactly the rows this call won.
//
// The two statements are safe without a transaction precisely because the token
// is unique per call: no other worker can select rows stamped with it.
func ClaimDueEscalations(ctx context.Context, db bun.IDB, claimToken string, now, leaseUntil time.Time) ([]*domain.AlertEscalation, error) {
	if _, err := db.ExecContext(ctx,
		`UPDATE alert_escalations
		    SET lease_owner = ?, lease_until = ?, updated_at = ?
		  WHERE status = ?
		    AND next_run_at <= ?
		    AND (lease_until IS NULL OR lease_until <= ?)`,
		claimToken, leaseUntil, now, domain.EscalationStatePending, now, now,
	); err != nil {
		return nil, err
	}

	var models []*AlertEscalationModel
	if err := db.NewSelect().Model(&models).
		Where("lease_owner = ?", claimToken).
		Where("status = ?", domain.EscalationStatePending).
		// Ordered by id as well as time: next_run_at is second-precision on
		// MariaDB, and a batch a human reads as a sequence needs a deterministic
		// tie-break (AGENTS.md rule 8).
		Order("next_run_at ASC", "id ASC").
		Scan(ctx); err != nil {
		return nil, err
	}

	out := make([]*domain.AlertEscalation, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}

// AdvanceEscalation moves a claimed row to the next step and releases the lease.
// The claimToken guard is what stops a worker whose lease expired mid-send from
// clobbering a row another worker has since taken.
func AdvanceEscalation(ctx context.Context, db bun.IDB, id int64, claimToken string, nextStep int, nextRunAt time.Time) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE alert_escalations
		    SET next_step = ?, next_run_at = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?
		  WHERE id = ? AND lease_owner = ? AND status = ?`,
		nextStep, nextRunAt, time.Now().UTC(), id, claimToken, domain.EscalationStatePending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// FinishEscalation marks a claimed row done or canceled, with the same
// claimToken guard as AdvanceEscalation.
func FinishEscalation(ctx context.Context, db bun.IDB, id int64, claimToken, status string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE alert_escalations
		    SET status = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?
		  WHERE id = ? AND lease_owner = ? AND status = ?`,
		status, time.Now().UTC(), id, claimToken, domain.EscalationStatePending,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// CancelEscalationByAlertID cancels a pending escalation regardless of any held
// lease. An acknowledgement must always win the race against a worker that is
// mid-step; the worker's own re-read of the alert status closes the remainder
// of that window.
func CancelEscalationByAlertID(ctx context.Context, db bun.IDB, alertID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE alert_escalations
		    SET status = ?, lease_owner = NULL, lease_until = NULL, updated_at = ?
		  WHERE alert_id = ? AND status = ?`,
		domain.EscalationStateCanceled, time.Now().UTC(), alertID, domain.EscalationStatePending,
	)
	return err
}
