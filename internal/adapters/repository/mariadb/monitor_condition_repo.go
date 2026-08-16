package mariadb

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// MonitorConditionRepo persists latest auxiliary conditions in MariaDB.
type MonitorConditionRepo struct{ db *bun.DB }

// NewMonitorConditionRepo creates a MariaDB-backed condition repository.
func NewMonitorConditionRepo(db *bun.DB) *MonitorConditionRepo {
	return &MonitorConditionRepo{db: db}
}

// Upsert inserts or replaces the latest state and notification cursor.
func (r *MonitorConditionRepo) Upsert(ctx context.Context, condition *domain.MonitorCondition) error {
	model := repository.MonitorConditionModelFromDomain(condition)
	_, err := r.db.NewInsert().Model(model).
		On("DUPLICATE KEY UPDATE").
		Set("state = VALUES(state)").
		Set("used_value = VALUES(used_value)").
		Set("limit_value = VALUES(limit_value)").
		Set("percent_value = VALUES(percent_value)").
		Set("threshold_value = VALUES(threshold_value)").
		Set("unit = VALUES(unit)").
		Set("resource = VALUES(resource)").
		Set("scope = VALUES(scope)").
		Set("source = VALUES(source)").
		Set("message = VALUES(message)").
		Set("observed_at = VALUES(observed_at)").
		Set("stale_after = VALUES(stale_after)").
		Set("last_success_at = VALUES(last_success_at)").
		Set("consecutive_state = VALUES(consecutive_state)").
		Set("consecutive_count = VALUES(consecutive_count)").
		Set("last_notified_state = VALUES(last_notified_state)").
		Set("last_notified_at = VALUES(last_notified_at)").
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
