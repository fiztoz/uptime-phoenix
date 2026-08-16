package sqlite

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorConditionRepo persists latest auxiliary conditions in SQLite.
type MonitorConditionRepo struct{ db *bun.DB }

// NewMonitorConditionRepo creates a SQLite-backed condition repository.
func NewMonitorConditionRepo(db *bun.DB) *MonitorConditionRepo {
	return &MonitorConditionRepo{db: db}
}

// Upsert inserts or replaces the latest state and notification cursor.
func (r *MonitorConditionRepo) Upsert(ctx context.Context, condition *domain.MonitorCondition) error {
	model := repository.MonitorConditionModelFromDomain(condition)
	_, err := r.db.NewInsert().Model(model).
		On("CONFLICT(monitor_id, kind) DO UPDATE").
		Set("state = EXCLUDED.state").
		Set("used_value = EXCLUDED.used_value").
		Set("limit_value = EXCLUDED.limit_value").
		Set("percent_value = EXCLUDED.percent_value").
		Set("threshold_value = EXCLUDED.threshold_value").
		Set("unit = EXCLUDED.unit").
		Set("resource = EXCLUDED.resource").
		Set("scope = EXCLUDED.scope").
		Set("source = EXCLUDED.source").
		Set("message = EXCLUDED.message").
		Set("observed_at = EXCLUDED.observed_at").
		Set("stale_after = EXCLUDED.stale_after").
		Set("last_success_at = EXCLUDED.last_success_at").
		Set("consecutive_state = EXCLUDED.consecutive_state").
		Set("consecutive_count = EXCLUDED.consecutive_count").
		Set("last_notified_state = EXCLUDED.last_notified_state").
		Set("last_notified_at = EXCLUDED.last_notified_at").
		Exec(ctx)
	return translateError(err)
}

// Get returns one condition by monitor and kind.
func (r *MonitorConditionRepo) Get(ctx context.Context, monitorID int64, kind string) (*domain.MonitorCondition, error) {
	model := new(repository.MonitorConditionModel)
	if err := r.db.NewSelect().Model(model).
		Where("monitor_id = ? AND kind = ?", monitorID, kind).
		Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	return model.ToDomain(), nil
}

// ListAll returns every latest condition ordered deterministically.
func (r *MonitorConditionRepo) ListAll(ctx context.Context) ([]*domain.MonitorCondition, error) {
	return r.list(ctx, nil)
}

// ListByMonitorIDs returns latest conditions for the specified monitor IDs.
func (r *MonitorConditionRepo) ListByMonitorIDs(ctx context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error) {
	if len(monitorIDs) == 0 {
		return []*domain.MonitorCondition{}, nil
	}
	return r.list(ctx, monitorIDs)
}

func (r *MonitorConditionRepo) list(ctx context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error) {
	var models []*repository.MonitorConditionModel
	query := r.db.NewSelect().Model(&models).Order("monitor_id ASC", "kind ASC")
	if monitorIDs != nil {
		query = query.Where("monitor_id IN (?)", bun.List(monitorIDs))
	}
	if err := query.Scan(ctx); err != nil {
		return nil, translateError(err)
	}
	out := make([]*domain.MonitorCondition, 0, len(models))
	for _, model := range models {
		out = append(out, model.ToDomain())
	}
	return out, nil
}

// DeleteKind deletes one condition kind for a monitor.
func (r *MonitorConditionRepo) DeleteKind(ctx context.Context, monitorID int64, kind string) error {
	_, err := r.db.NewDelete().Model((*repository.MonitorConditionModel)(nil)).
		Where("monitor_id = ? AND kind = ?", monitorID, kind).
		Exec(ctx)
	return translateError(err)
}

// DeleteByMonitor deletes all conditions for a monitor.
func (r *MonitorConditionRepo) DeleteByMonitor(ctx context.Context, monitorID int64) error {
	_, err := r.db.NewDelete().Model((*repository.MonitorConditionModel)(nil)).
		Where("monitor_id = ?", monitorID).
		Exec(ctx)
	return translateError(err)
}

var _ ports.MonitorConditionRepository = (*MonitorConditionRepo)(nil)
