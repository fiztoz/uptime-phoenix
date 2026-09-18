package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AlertStore implements assignment-scoped alert persistence for both SQL engines.
type AlertStore struct {
	db        *bun.DB
	scope     AuxiliaryScope
	translate func(error) error
}

// NewAlertStore creates the local compatibility view.
func NewAlertStore(db *bun.DB, translate func(error) error) *AlertStore {
	return &AlertStore{db: db, translate: translate}
}

var _ ports.AlertRepository = (*AlertStore)(nil)
var _ ports.RegionalAlertRepository = (*AlertStore)(nil)

// ForAssignment confines reads and writes to this assignment, including historical IDs.
func (r *AlertStore) ForAssignment(probeID string, generation int64) (ports.AlertRepository, error) {
	scope, err := NewAuxiliaryScope(probeID, generation)
	if err != nil {
		return nil, err
	}
	return &AlertStore{db: r.db, scope: scope, translate: r.translate}, nil
}

func (r *AlertStore) filter(q *bun.SelectQuery) *bun.SelectQuery {
	if r.scope.ProbeID != "" {
		return q.Where("probe_id = ? AND assignment_generation = ?", r.scope.ProbeID, r.scope.Generation)
	}
	return q.Where("probe_id = ?", domain.LocalProbeID)
}

// Create inserts a firing incident without borrowing an earlier generation.
func (r *AlertStore) Create(ctx context.Context, a *domain.Alert) error {
	if a == nil || a.MonitorID < 1 {
		return domain.ErrValidation
	}
	scope, err := r.scope.Resolve(ctx, r.db, a.MonitorID)
	if err != nil {
		return r.translate(err)
	}
	if (a.ProbeID != "" && a.ProbeID != scope.ProbeID) ||
		(a.AssignmentGeneration != 0 && a.AssignmentGeneration != scope.Generation) {
		return domain.ErrValidation
	}
	if a.Status != domain.AlertStatusFiring || a.OpenMonitorID == nil || *a.OpenMonitorID != a.MonitorID || a.AckToken == "" {
		return domain.ErrValidation
	}
	m := AlertModelFromDomain(a)
	m.ProbeID, m.AssignmentGeneration = scope.ProbeID, scope.Generation
	now := time.Now().UTC()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	if m.UpdatedAt.IsZero() {
		m.UpdatedAt = now
	}
	if m.FiredAt.IsZero() {
		m.FiredAt = now
	}
	normalizeAlertTimes(m)
	if _, err := r.db.NewInsert().Model(m).Exec(ctx); err != nil {
		return r.translate(err)
	}
	*a = *m.ToDomain()
	return nil
}

// Update changes lifecycle state only. Identity, token and outage start are immutable.
// Conditional writes prevent a delayed acknowledgement from reopening a resolved incident.
func (r *AlertStore) Update(ctx context.Context, a *domain.Alert) error {
	if a == nil || a.ID < 1 {
		return domain.ErrValidation
	}
	m := AlertModelFromDomain(a)
	if m.ProbeID == "" {
		m.ProbeID = domain.LocalProbeID
	}
	if m.AssignmentGeneration == 0 {
		m.AssignmentGeneration = 1
	}
	if r.scope.ProbeID != "" {
		if m.ProbeID != r.scope.ProbeID || m.AssignmentGeneration != r.scope.Generation {
			return ports.ErrNotFound
		}
	} else if m.ProbeID != domain.LocalProbeID {
		return ports.ErrNotFound
	}
	m.UpdatedAt = time.Now().UTC()
	normalizeAlertTimes(m)
	q := r.db.NewUpdate().Model(m).WherePK().
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", m.MonitorID, m.ProbeID, m.AssignmentGeneration)
	switch m.Status {
	case domain.AlertStatusAcked:
		if m.AckedAt == nil || m.OpenMonitorID == nil || *m.OpenMonitorID != m.MonitorID {
			return domain.ErrValidation
		}
		q = q.Column("status", "acked_at", "acked_by_user_id", "updated_at").Where("status = ?", domain.AlertStatusFiring)
	case domain.AlertStatusResolved:
		if m.ResolvedAt == nil || m.OpenMonitorID != nil {
			return domain.ErrValidation
		}
		q = q.Column("status", "resolved_at", "open_monitor_id", "updated_at").
			Where("status IN (?, ?)", domain.AlertStatusFiring, domain.AlertStatusAcked)
	default:
		return domain.ErrValidation
	}
	res, err := q.Exec(ctx)
	if err != nil {
		return r.translate(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("alert update affected rows: %w", err)
	}
	stored, err := r.GetByID(ctx, a.ID)
	if err != nil {
		return err
	}
	if stored.MonitorID != m.MonitorID || stored.ProbeID != m.ProbeID || stored.AssignmentGeneration != m.AssignmentGeneration {
		return ports.ErrNotFound
	}
	if n == 0 && stored.Status != m.Status {
		return ports.ErrConflict
	}
	*a = *stored
	return nil
}

// GetByID reads one incident within this repository's scope.
func (r *AlertStore) GetByID(ctx context.Context, id int64) (*domain.Alert, error) {
	m := new(AlertModel)
	if err := r.filter(r.db.NewSelect().Model(m)).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, r.translate(err)
	}
	normalizeAlertTimes(m)
	return m.ToDomain(), nil
}

// GetByAckToken reads a local deep-link token. Remote links are not supported.
func (r *AlertStore) GetByAckToken(ctx context.Context, token string) (*domain.Alert, error) {
	m := new(AlertModel)
	if err := r.filter(r.db.NewSelect().Model(m)).Where("probe_id = ?", domain.LocalProbeID).
		Where("ack_token = ?", token).Scan(ctx); err != nil {
		return nil, r.translate(err)
	}
	normalizeAlertTimes(m)
	return m.ToDomain(), nil
}

// GetOpenByMonitorID reads the bound assignment, or the current local assignment.
func (r *AlertStore) GetOpenByMonitorID(ctx context.Context, monitorID int64) (*domain.Alert, error) {
	m := new(AlertModel)
	q := r.scope.Filter(r.db.NewSelect().Model(m), "alert_model").Where("open_monitor_id = ?", monitorID)
	if err := q.Scan(ctx); err != nil {
		return nil, r.translate(err)
	}
	normalizeAlertTimes(m)
	return m.ToDomain(), nil
}

// List returns scoped history with deterministic newest-first order.
func (r *AlertStore) List(ctx context.Context, filter ports.AlertFilter) ([]*domain.Alert, error) {
	var models []*AlertModel
	q := r.filter(r.db.NewSelect().Model(&models))
	if filter.RestrictToMonitorIDs {
		if len(filter.MonitorIDs) == 0 {
			return []*domain.Alert{}, nil
		}
		q = q.Where("monitor_id IN (?)", bun.List(filter.MonitorIDs))
	}
	if filter.MonitorID != nil {
		q = q.Where("monitor_id = ?", *filter.MonitorID)
	}
	if filter.OpenOnly {
		q = q.Where("status IN (?, ?)", domain.AlertStatusFiring, domain.AlertStatusAcked)
	} else if len(filter.Statuses) > 0 {
		q = q.Where("status IN (?)", bun.List(filter.Statuses))
	}
	if filter.Limit > 0 {
		q = q.Limit(filter.Limit)
	}
	if filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	if err := q.Order("fired_at DESC", "id DESC").Scan(ctx); err != nil {
		return nil, r.translate(err)
	}
	out := make([]*domain.Alert, len(models))
	for i, m := range models {
		normalizeAlertTimes(m)
		out[i] = m.ToDomain()
	}
	return out, nil
}

func normalizeAlertTimes(m *AlertModel) {
	m.FiredAt, m.CreatedAt, m.UpdatedAt = m.FiredAt.UTC(), m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	if m.AckedAt != nil {
		at := m.AckedAt.UTC()
		m.AckedAt = &at
	}
	if m.ResolvedAt != nil {
		at := m.ResolvedAt.UTC()
		m.ResolvedAt = &at
	}
}
