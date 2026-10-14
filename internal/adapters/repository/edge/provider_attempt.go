package edge

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.SourceProviderAttemptJournal = (*Store)(nil)

// providerAttemptRow is one admitted availability provider grant. The row is an
// I/O fact journal: only an exact caller release may finish a grant, and no
// timer, lease reclaim, restart or reset can clear an unresolved one.
type providerAttemptRow struct {
	bun.BaseModel `bun:"table:edge_provider_attempts"`
	AttemptID     string `bun:"attempt_id,pk"`
	DeliveryID    string
	Attempt       int64
	MonitorID     int64
	State         domain.ProviderAttemptState
	GrantedAt     int64
	FinishedAt    *int64
}

func validProviderAttemptState(state domain.ProviderAttemptState) bool {
	return state == domain.ProviderAttemptSucceeded || state == domain.ProviderAttemptAmbiguous || state == domain.ProviderAttemptNotStarted
}

// insertProviderAttempt journals one opaque final-authorization grant in the
// same transaction as the stored claim, config, gate and lifecycle checks. One
// grant covers authorization plus provider I/O for one claimed attempt: repeat
// authorizations of the same claim return that same grant instead of opening a
// second unaccounted I/O permission, and a finished grant refuses new starts.
func insertProviderAttempt(ctx context.Context, tx bun.Tx, deliveryID string, attempt, monitorID int64, now time.Time) (string, error) {
	if !domain.ValidHubID(deliveryID) || attempt < 1 || monitorID <= 0 || now.IsZero() {
		return "", domain.ErrValidation
	}
	var existing providerAttemptRow
	err := tx.NewSelect().Model(&existing).Where("delivery_id = ? AND attempt = ?", deliveryID, attempt).Scan(ctx)
	if err == nil {
		if existing.State != domain.ProviderAttemptInFlight {
			// This claim attempt's provider I/O is already resolved. A new start
			// needs a new claim attempt and its own grant.
			return "", ports.ErrConflict
		}
		return existing.AttemptID, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	grant, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	row := providerAttemptRow{AttemptID: grant.String(), DeliveryID: deliveryID, Attempt: attempt,
		MonitorID: monitorID, State: domain.ProviderAttemptInFlight, GrantedAt: now.UnixMicro()}
	if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
		return "", err
	}
	return row.AttemptID, nil
}

// FinishSourceProviderAttempt records the exact provider outcome for one grant.
// A matching release may commit after claim or lease expiry and never affects a
// replacement grant. Known states are immutable: a crashed, unknown or errored
// attempt stays ambiguous or in_flight and keeps blocking quiescence and drain.
func (s *Store) FinishSourceProviderAttempt(ctx context.Context, attemptID string, state domain.ProviderAttemptState) error {
	if len(attemptID) < 1 || len(attemptID) > 128 || !validProviderAttemptState(state) {
		return domain.ErrValidation
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, _ domain.EdgeIdentity) error {
		var row providerAttemptRow
		if err := tx.NewSelect().Model(&row).Where("attempt_id = ?", attemptID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return err
		}
		if row.State != domain.ProviderAttemptInFlight {
			if row.State == state {
				// An identical retry of a completed release is a no-op.
				return nil
			}
			return ports.ErrConflict
		}
		now := time.Now().UTC().Truncate(time.Microsecond)
		result, err := tx.NewUpdate().Model((*providerAttemptRow)(nil)).
			Set("state = ?", state).Set("finished_at = ?", now.UnixMicro()).
			Where("attempt_id = ? AND state = ?", attemptID, domain.ProviderAttemptInFlight).Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ports.ErrConflict
		}
		return nil
	})
}
