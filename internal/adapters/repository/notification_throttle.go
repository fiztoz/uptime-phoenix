package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type notificationThrottleModel struct {
	bun.BaseModel        `bun:"table:notification_throttles"`
	MonitorID            int64      `bun:"monitor_id,pk"`
	ProbeID              string     `bun:"probe_id,pk"`
	AssignmentGeneration int64      `bun:"assignment_generation,pk"`
	LastAttemptAt        *time.Time `bun:"last_attempt_at"`
}

// NotificationThrottleStore implements durable attempt throttles on either DB.
type NotificationThrottleStore struct{ db *bun.DB }

// NewNotificationThrottleStore creates a shared MariaDB/SQLite implementation.
func NewNotificationThrottleStore(db *bun.DB) *NotificationThrottleStore {
	return &NotificationThrottleStore{db: db}
}

var _ ports.NotificationThrottleRepository = (*NotificationThrottleStore)(nil)

// Reserve serializes the eligibility check and cursor update across connections.
// The first statement is a write so SQLite never upgrades a stale read snapshot.
// Provider I/O happens only after this transaction has committed.
func (r *NotificationThrottleStore) Reserve(ctx context.Context, key domain.NotificationThrottleKey, at time.Time, interval time.Duration) (bool, error) {
	if err := validateThrottleKey(key); err != nil {
		return false, err
	}
	if at.IsZero() || interval < 0 {
		return false, fmt.Errorf("invalid notification throttle time: %w", domain.ErrValidation)
	}
	at = at.UTC()
	reserved := false
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		model := notificationThrottleModel{MonitorID: key.MonitorID, ProbeID: key.ProbeID, AssignmentGeneration: key.AssignmentGeneration}
		insert := tx.NewInsert().Model(&model)
		if tx.Dialect().Name() == dialect.MySQL {
			insert = insert.On("DUPLICATE KEY UPDATE").Set("monitor_id = VALUES(monitor_id)")
		} else {
			insert = insert.On("CONFLICT (monitor_id, probe_id, assignment_generation) DO NOTHING")
		}
		if _, err := insert.Exec(ctx); err != nil {
			return fmt.Errorf("lock notification throttle: %w", probeRegistryError(err))
		}
		query := tx.NewSelect().Model(&model).WherePK()
		if tx.Dialect().Name() == dialect.MySQL {
			query = query.For("UPDATE")
		}
		if err := query.Scan(ctx); err != nil {
			return err
		}
		if model.LastAttemptAt != nil {
			last := model.LastAttemptAt.UTC()
			if interval > 0 && at.Sub(last) < interval {
				return nil
			}
			if last.After(at) {
				at = last
			}
		}
		model.LastAttemptAt = &at
		if _, err := tx.NewUpdate().Model(&model).Column("last_attempt_at").WherePK().Exec(ctx); err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("reserve notification attempt: %w", err)
	}
	return reserved, nil
}

// Clear forgets one assignment after recovery; other regions/generations survive.
func (r *NotificationThrottleStore) Clear(ctx context.Context, key domain.NotificationThrottleKey) error {
	if err := validateThrottleKey(key); err != nil {
		return err
	}
	_, err := r.db.NewDelete().TableExpr("notification_throttles").
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", key.MonitorID, key.ProbeID, key.AssignmentGeneration).Exec(ctx)
	return err
}

func validateThrottleKey(key domain.NotificationThrottleKey) error {
	if key.MonitorID <= 0 || key.AssignmentGeneration < 1 || (key.ProbeID != domain.LocalProbeID && !validRemoteProbeID(key.ProbeID)) {
		return fmt.Errorf("invalid notification throttle identity: %w", domain.ErrValidation)
	}
	return nil
}
