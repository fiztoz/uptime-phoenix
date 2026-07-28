package sqlite

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AlertRepo implements ports.AlertRepository backed by SQLite.
type AlertRepo struct{ db *bun.DB }

// NewAlertRepo creates a SQLite-backed alert repository.
func NewAlertRepo(db *bun.DB) *AlertRepo { return &AlertRepo{db: db} }

var _ ports.AlertRepository = (*AlertRepo)(nil)

// Create inserts a new alert and assigns ID/timestamps.
func (r *AlertRepo) Create(ctx context.Context, a *domain.Alert) error {
	m := repository.AlertModelFromDomain(a)
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
	_, err := r.db.NewInsert().Model(m).Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	a.ID = m.ID
	a.CreatedAt = m.CreatedAt
	a.UpdatedAt = m.UpdatedAt
	a.FiredAt = m.FiredAt
	return nil
}

// Update persists all mutable alert fields.
func (r *AlertRepo) Update(ctx context.Context, a *domain.Alert) error {
	m := repository.AlertModelFromDomain(a)
	m.UpdatedAt = time.Now().UTC()
	_, err := r.db.NewUpdate().Model(m).WherePK().Exec(ctx)
	if err != nil {
		return translateError(err)
	}
	a.UpdatedAt = m.UpdatedAt
	return nil
}

// GetByID returns one alert by primary key.
func (r *AlertRepo) GetByID(ctx context.Context, id int64) (*domain.Alert, error) {
	m := new(repository.AlertModel)
	if err := r.db.NewSelect().Model(m).Where("id = ?", id).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// GetByAckToken looks up an alert by deep-link token.
func (r *AlertRepo) GetByAckToken(ctx context.Context, token string) (*domain.Alert, error) {
	m := new(repository.AlertModel)
	if err := r.db.NewSelect().Model(m).Where("ack_token = ?", token).Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// GetOpenByMonitorID returns the firing/acked alert for a monitor.
func (r *AlertRepo) GetOpenByMonitorID(ctx context.Context, monitorID int64) (*domain.Alert, error) {
	m := new(repository.AlertModel)
	err := r.db.NewSelect().Model(m).
		Where("open_monitor_id = ?", monitorID).
		Scan(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return m.ToDomain(), nil
}

// List returns alerts matching the filter, newest first.
func (r *AlertRepo) List(ctx context.Context, filter ports.AlertFilter) ([]*domain.Alert, error) {
	var models []*repository.AlertModel
	q := r.db.NewSelect().Model(&models)

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
		return nil, translateError(err)
	}
	out := make([]*domain.Alert, len(models))
	for i, m := range models {
		out[i] = m.ToDomain()
	}
	return out, nil
}
