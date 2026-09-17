package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type probeAssignmentHistoryModel struct {
	bun.BaseModel `bun:"table:monitor_probe_assignment_history,alias:ah"`
	ID            int64               `bun:"id,pk,autoincrement"`
	MonitorID     int64               `bun:"monitor_id"`
	ProbeID       string              `bun:"probe_id"`
	Generation    int64               `bun:"generation"`
	Revision      int64               `bun:"revision"`
	HealthPolicy  domain.HealthPolicy `bun:"health_policy"`
	StartedAt     time.Time           `bun:"started_at"`
	EndedAt       *time.Time          `bun:"ended_at"`
}

// ListHistory returns the actual assignment/policy timeline overlapping [from,to).
// Retired generations remain readable; a missing timeline is not synthesized.
func (r *ProbeAssignmentStore) ListHistory(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.AssignmentInterval, error) {
	from, to = from.UTC(), to.UTC()
	if monitorID <= 0 || from.IsZero() || to.IsZero() || !from.Before(to) {
		return nil, fmt.Errorf("assignment history window: %w", domain.ErrValidation)
	}
	var rows []probeAssignmentHistoryModel
	if err := r.db.NewSelect().Model(&rows).
		Where("monitor_id = ? AND started_at < ?", monitorID, to).
		Where("ended_at IS NULL OR ended_at > ?", from).
		Order("started_at ASC", "id ASC").Scan(ctx); err != nil {
		return nil, fmt.Errorf("list assignment history: %w", err)
	}
	out := make([]domain.AssignmentInterval, 0, len(rows))
	for _, row := range rows {
		interval := domain.AssignmentInterval{
			ProbeID: row.ProbeID, Generation: row.Generation, Revision: row.Revision,
			Policy: row.HealthPolicy, From: row.StartedAt.UTC(),
		}
		if row.EndedAt != nil {
			interval.To = row.EndedAt.UTC()
		}
		out = append(out, interval)
	}
	return out, nil
}

// Assignment writes hold the set lock. Every revision closes the previous whole
// snapshot and opens another at one common boundary, including policy-only edits.
func writeAssignmentHistory(ctx context.Context, tx bun.Tx, monitorID, revision int64, policy domain.HealthPolicy, at time.Time) error {
	if _, err := tx.NewUpdate().Table("monitor_probe_assignment_history").
		Set("ended_at = ?", at).Where("monitor_id = ? AND ended_at IS NULL", monitorID).Exec(ctx); err != nil {
		return fmt.Errorf("close assignment history: %w", err)
	}
	var assignments []probeAssignmentModel
	if err := tx.NewSelect().Model(&assignments).Where("monitor_id = ? AND active = ?", monitorID, true).
		Order("probe_id ASC").Scan(ctx); err != nil {
		return err
	}
	rows := make([]probeAssignmentHistoryModel, 0, len(assignments))
	for _, assignment := range assignments {
		rows = append(rows, probeAssignmentHistoryModel{
			MonitorID: monitorID, ProbeID: assignment.ProbeID, Generation: assignment.Generation,
			Revision: revision, HealthPolicy: policy, StartedAt: at,
		})
	}
	if len(rows) == 0 {
		return fmt.Errorf("assignment history requires members: %w", domain.ErrValidation)
	}
	if _, err := tx.NewInsert().Model(&rows).Exec(ctx); err != nil {
		return fmt.Errorf("open assignment history: %w", err)
	}
	// A membership/policy change changes the overall minute even with no check.
	return markDirtyTx(ctx, tx, []domain.DirtyBucket{{
		MonitorID: monitorID, ProbeID: rows[0].ProbeID,
		Resolution: domain.DirtyResolutionOverall, Bucket: at.Truncate(time.Minute),
	}})
}

// Both engines persist microseconds here. Strictly increasing boundaries keep
// fast consecutive revisions and a backward hub clock from overlapping history.
func assignmentChangeTime(previous time.Time) time.Time {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(previous) {
		return previous.UTC().Truncate(time.Microsecond).Add(time.Microsecond)
	}
	return now
}
