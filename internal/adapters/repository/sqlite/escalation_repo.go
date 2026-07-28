package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EscalationPolicyRepo implements ports.EscalationPolicyRepository on SQLite.
type EscalationPolicyRepo struct{ db *bun.DB }

// NewEscalationPolicyRepo creates a SQLite-backed escalation policy repository.
func NewEscalationPolicyRepo(db *bun.DB) *EscalationPolicyRepo {
	return &EscalationPolicyRepo{db: db}
}

var _ ports.EscalationPolicyRepository = (*EscalationPolicyRepo)(nil)

// Create inserts the policy and its whole step ladder in one transaction.
func (r *EscalationPolicyRepo) Create(ctx context.Context, p *domain.EscalationPolicy) error {
	m := repository.EscalationPolicyModelFromDomain(p)
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now

	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(m).Exec(ctx); err != nil {
			return err
		}
		return repository.InsertEscalationSteps(ctx, tx, m.ID, p.Steps)
	})
	if err != nil {
		return translateError(err)
	}
	p.ID = m.ID
	p.CreatedAt = m.CreatedAt
	p.UpdatedAt = m.UpdatedAt
	for i := range p.Steps {
		p.Steps[i].PolicyID = m.ID
	}
	return nil
}

// Update rewrites the policy and REPLACES its entire step list. See the port
// doc: steps absent from p.Steps are deleted.
func (r *EscalationPolicyRepo) Update(ctx context.Context, p *domain.EscalationPolicy) error {
	m := repository.EscalationPolicyModelFromDomain(p)
	m.UpdatedAt = time.Now().UTC()

	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		res, err := tx.NewUpdate().Model(m).
			Column("name", "description", "enabled", "updated_at").
			WherePK().Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			// Distinguish "no such policy" from "nothing changed": SQLite reports
			// 0 for an UPDATE whose values are identical, so confirm existence.
			exists, existsErr := tx.NewSelect().
				Model((*repository.EscalationPolicyModel)(nil)).
				Where("id = ?", m.ID).Exists(ctx)
			if existsErr != nil {
				return existsErr
			}
			if !exists {
				return sql.ErrNoRows
			}
		}
		if _, err := tx.NewDelete().
			Model((*repository.EscalationStepModel)(nil)).
			Where("policy_id = ?", m.ID).Exec(ctx); err != nil {
			return err
		}
		return repository.InsertEscalationSteps(ctx, tx, m.ID, p.Steps)
	})
	if err != nil {
		return translateError(err)
	}
	p.UpdatedAt = m.UpdatedAt
	return nil
}

// GetByID returns one policy with its steps in StepOrder ascending.
func (r *EscalationPolicyRepo) GetByID(ctx context.Context, id int64) (*domain.EscalationPolicy, error) {
	m := new(repository.EscalationPolicyModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	p := m.ToDomain()
	steps, err := repository.LoadEscalationSteps(ctx, r.db, []int64{id})
	if err != nil {
		return nil, translateError(err)
	}
	p.Steps = steps[id]
	return p, nil
}

// List returns every policy with steps populated, ordered by id.
func (r *EscalationPolicyRepo) List(ctx context.Context) ([]*domain.EscalationPolicy, error) {
	var models []*repository.EscalationPolicyModel
	if err := r.db.NewSelect().Model(&models).Order("id ASC").Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.EscalationPolicy, len(models))
	ids := make([]int64, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
		ids[i] = m.ID
	}
	if len(ids) == 0 {
		return out, nil
	}
	steps, err := repository.LoadEscalationSteps(ctx, r.db, ids)
	if err != nil {
		return nil, translateError(err)
	}
	for _, p := range out {
		p.Steps = steps[p.ID]
	}
	return out, nil
}

// Delete removes the policy; steps, links and assignments cascade.
func (r *EscalationPolicyRepo) Delete(ctx context.Context, id int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.EscalationPolicyModel)(nil)).
		Where("id = ?", id).Exec(ctx)
	return translateError(err)
}

// EscalationAssignmentRepo implements ports.EscalationAssignmentRepository on SQLite.
type EscalationAssignmentRepo struct{ db *bun.DB }

// NewEscalationAssignmentRepo creates a SQLite-backed assignment repository.
func NewEscalationAssignmentRepo(db *bun.DB) *EscalationAssignmentRepo {
	return &EscalationAssignmentRepo{db: db}
}

var _ ports.EscalationAssignmentRepository = (*EscalationAssignmentRepo)(nil)

// AssignMonitor points a monitor at a policy, replacing any prior assignment.
func (r *EscalationAssignmentRepo) AssignMonitor(ctx context.Context, monitorID, policyID int64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO escalation_policy_monitors (monitor_id, policy_id) VALUES (?, ?)
		 ON CONFLICT (monitor_id) DO UPDATE SET policy_id = excluded.policy_id`,
		monitorID, policyID,
	)
	return translateError(err)
}

// UnassignMonitor removes the monitor's assignment.
func (r *EscalationAssignmentRepo) UnassignMonitor(ctx context.Context, monitorID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.EscalationPolicyMonitorModel)(nil)).
		Where("monitor_id = ?", monitorID).Exec(ctx)
	return translateError(err)
}

// PolicyIDForMonitor returns the monitor's direct assignment, or ErrNotFound.
func (r *EscalationAssignmentRepo) PolicyIDForMonitor(ctx context.Context, monitorID int64) (int64, error) {
	m := new(repository.EscalationPolicyMonitorModel)
	if err := r.db.NewSelect().Model(m).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
		return 0, translateError(err)
	}
	return m.PolicyID, nil
}

// AssignGroup points a monitor group at a policy, replacing any prior assignment.
func (r *EscalationAssignmentRepo) AssignGroup(ctx context.Context, groupID, policyID int64) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO escalation_policy_groups (group_id, policy_id) VALUES (?, ?)
		 ON CONFLICT (group_id) DO UPDATE SET policy_id = excluded.policy_id`,
		groupID, policyID,
	)
	return translateError(err)
}

// UnassignGroup removes the group's assignment.
func (r *EscalationAssignmentRepo) UnassignGroup(ctx context.Context, groupID int64) error {
	_, err := r.db.NewDelete().
		Model((*repository.EscalationPolicyGroupModel)(nil)).
		Where("group_id = ?", groupID).Exec(ctx)
	return translateError(err)
}

// PolicyIDForGroup returns the group's direct assignment, or ErrNotFound.
func (r *EscalationAssignmentRepo) PolicyIDForGroup(ctx context.Context, groupID int64) (int64, error) {
	m := new(repository.EscalationPolicyGroupModel)
	if err := r.db.NewSelect().Model(m).Where("group_id = ?", groupID).Scan(ctx); err != nil {
		return 0, translateError(err)
	}
	return m.PolicyID, nil
}

// ListMonitorsByPolicy returns monitor IDs directly assigned to the policy.
func (r *EscalationAssignmentRepo) ListMonitorsByPolicy(ctx context.Context, policyID int64) ([]int64, error) {
	var models []repository.EscalationPolicyMonitorModel
	if err := r.db.NewSelect().
		Model(&models).
		Where("policy_id = ?", policyID).
		OrderExpr("monitor_id ASC").
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.MonitorID
	}
	return ids, nil
}

// ListGroupsByPolicy returns group IDs directly assigned to the policy.
func (r *EscalationAssignmentRepo) ListGroupsByPolicy(ctx context.Context, policyID int64) ([]int64, error) {
	var models []repository.EscalationPolicyGroupModel
	if err := r.db.NewSelect().
		Model(&models).
		Where("policy_id = ?", policyID).
		OrderExpr("group_id ASC").
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	ids := make([]int64, len(models))
	for i, m := range models {
		ids[i] = m.GroupID
	}
	return ids, nil
}

// AlertEscalationRepo implements ports.AlertEscalationRepository on SQLite.
type AlertEscalationRepo struct{ db *bun.DB }

// NewAlertEscalationRepo creates a SQLite-backed alert escalation repository.
func NewAlertEscalationRepo(db *bun.DB) *AlertEscalationRepo {
	return &AlertEscalationRepo{db: db}
}

var _ ports.AlertEscalationRepository = (*AlertEscalationRepo)(nil)

// Create starts the ladder for an alert.
func (r *AlertEscalationRepo) Create(ctx context.Context, e *domain.AlertEscalation) error {
	m := repository.AlertEscalationModelFromDomain(e)
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.NextRunAt.IsZero() {
		m.NextRunAt = now
	}
	if _, err := r.db.NewInsert().Model(m).Exec(ctx); err != nil {
		return translateError(err)
	}
	e.ID = m.ID
	e.CreatedAt = m.CreatedAt
	e.UpdatedAt = m.UpdatedAt
	e.NextRunAt = m.NextRunAt
	return nil
}

// GetByAlertID returns the escalation for an alert.
func (r *AlertEscalationRepo) GetByAlertID(ctx context.Context, alertID int64) (*domain.AlertEscalation, error) {
	m := new(repository.AlertEscalationModel)
	if err := r.db.NewSelect().Model(m).Where("alert_id = ?", alertID).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// ListByAlertIDs returns the escalations for many alerts in one query.
func (r *AlertEscalationRepo) ListByAlertIDs(ctx context.Context, alertIDs []int64) (map[int64]*domain.AlertEscalation, error) {
	out, err := repository.ListEscalationsByAlertIDs(ctx, r.db, alertIDs)
	if err != nil {
		return nil, translateError(err)
	}
	return out, nil
}

// ClaimDue leases every due pending row for claimToken and returns the winners.
func (r *AlertEscalationRepo) ClaimDue(ctx context.Context, claimToken string, now, leaseUntil time.Time) ([]*domain.AlertEscalation, error) {
	return repository.ClaimDueEscalations(ctx, r.db, claimToken, now, leaseUntil)
}

// Advance moves a claimed row to the next step and releases the lease.
func (r *AlertEscalationRepo) Advance(ctx context.Context, id int64, claimToken string, nextStep int, nextRunAt time.Time) (bool, error) {
	return repository.AdvanceEscalation(ctx, r.db, id, claimToken, nextStep, nextRunAt)
}

// Finish marks a claimed row done or canceled and releases the lease.
func (r *AlertEscalationRepo) Finish(ctx context.Context, id int64, claimToken, status string) (bool, error) {
	return repository.FinishEscalation(ctx, r.db, id, claimToken, status)
}

// CancelByAlertID cancels the alert's pending escalation, ignoring any lease.
func (r *AlertEscalationRepo) CancelByAlertID(ctx context.Context, alertID int64) error {
	return repository.CancelEscalationByAlertID(ctx, r.db, alertID)
}
