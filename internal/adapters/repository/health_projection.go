package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type monitorHealthStateModel struct {
	bun.BaseModel     `bun:"table:monitor_health_state,alias:hs"`
	MonitorID         int64     `bun:"monitor_id,pk"`
	HealthPolicy      string    `bun:"health_policy"`
	ProjectionVersion int64     `bun:"projection_version"`
	Status            int       `bun:"status"`
	Reason            string    `bun:"reason"`
	AssignedCount     int       `bun:"assigned_count"`
	UpCount           int       `bun:"up_count"`
	DownCount         int       `bun:"down_count"`
	PendingCount      int       `bun:"pending_count"`
	UnknownCount      int       `bun:"unknown_count"`
	MaintenanceCount  int       `bun:"maintenance_count"`
	PausedCount       int       `bun:"paused_count"`
	LastTransitionAt  time.Time `bun:"last_transition_at"`
	AsOf              time.Time `bun:"as_of"`
	UpdatedAt         time.Time `bun:"updated_at"`
}

type monitorHealthHistoryModel struct {
	bun.BaseModel     `bun:"table:monitor_health_history,alias:hh"`
	ID                int64      `bun:"id,pk,autoincrement"`
	MonitorID         int64      `bun:"monitor_id"`
	StartedAt         time.Time  `bun:"started_at"`
	EndedAt           *time.Time `bun:"ended_at"`
	Status            int        `bun:"status"`
	Reason            string     `bun:"reason"`
	Cause             string     `bun:"cause"`
	PolicyRevision    int64      `bun:"policy_revision"`
	HealthPolicy      string     `bun:"health_policy"`
	ProjectionVersion int64      `bun:"projection_version"`
	AssignedCount     int        `bun:"assigned_count"`
	UpCount           int        `bun:"up_count"`
	DownCount         int        `bun:"down_count"`
	PendingCount      int        `bun:"pending_count"`
	UnknownCount      int        `bun:"unknown_count"`
	MaintenanceCount  int        `bun:"maintenance_count"`
	PausedCount       int        `bun:"paused_count"`
}

type probeDirtyBucketModel struct {
	bun.BaseModel `bun:"table:probe_dirty_buckets,alias:dirty"`
	MonitorID     int64     `bun:"monitor_id,pk"`
	ProbeID       string    `bun:"probe_id,pk"`
	Resolution    string    `bun:"resolution,pk"`
	Bucket        time.Time `bun:"bucket,pk"`
}

var _ ports.MonitorHealthProjectionRepository = (*RegionalCommitStore)(nil)

func (m monitorHealthStateModel) state() domain.MonitorHealthState {
	return domain.MonitorHealthState{
		MonitorID: m.MonitorID, Policy: domain.HealthPolicy(m.HealthPolicy),
		ProjectionVersion: m.ProjectionVersion, Status: domain.Status(m.Status), Reason: m.Reason,
		Counts: domain.ProbeHealthCounts{
			Assigned: m.AssignedCount, Up: m.UpCount, Down: m.DownCount, Pending: m.PendingCount,
			Unknown: m.UnknownCount, Maintenance: m.MaintenanceCount, Paused: m.PausedCount,
		},
		LastTransitionAt: m.LastTransitionAt.UTC(), AsOf: m.AsOf.UTC(),
	}
}

func healthStateModel(state domain.MonitorHealthState, now time.Time) monitorHealthStateModel {
	return monitorHealthStateModel{
		MonitorID: state.MonitorID, HealthPolicy: string(state.Policy),
		ProjectionVersion: state.ProjectionVersion, Status: int(state.Status), Reason: state.Reason,
		AssignedCount: state.Counts.Assigned, UpCount: state.Counts.Up, DownCount: state.Counts.Down,
		PendingCount: state.Counts.Pending, UnknownCount: state.Counts.Unknown,
		MaintenanceCount: state.Counts.Maintenance, PausedCount: state.Counts.Paused,
		LastTransitionAt: state.LastTransitionAt.UTC(), AsOf: state.AsOf.UTC(), UpdatedAt: now,
	}
}

func (m monitorHealthHistoryModel) interval() domain.MonitorHealthInterval {
	out := domain.MonitorHealthInterval{
		From: m.StartedAt.UTC(), Status: domain.Status(m.Status), Reason: m.Reason, Cause: m.Cause,
		Policy: domain.HealthPolicy(m.HealthPolicy), PolicyRevision: m.PolicyRevision,
		Counts: domain.ProbeHealthCounts{
			Assigned: m.AssignedCount, Up: m.UpCount, Down: m.DownCount, Pending: m.PendingCount,
			Unknown: m.UnknownCount, Maintenance: m.MaintenanceCount, Paused: m.PausedCount,
		},
	}
	if m.EndedAt != nil {
		out.To = m.EndedAt.UTC()
	}
	return out
}

func healthHistoryModel(monitorID, version int64, interval domain.MonitorHealthInterval) monitorHealthHistoryModel {
	row := monitorHealthHistoryModel{
		MonitorID: monitorID, StartedAt: interval.From.UTC(), Status: int(interval.Status),
		Reason: interval.Reason, Cause: interval.Cause, PolicyRevision: interval.PolicyRevision,
		HealthPolicy: string(interval.Policy), ProjectionVersion: version,
		AssignedCount: interval.Counts.Assigned, UpCount: interval.Counts.Up, DownCount: interval.Counts.Down,
		PendingCount: interval.Counts.Pending, UnknownCount: interval.Counts.Unknown,
		MaintenanceCount: interval.Counts.Maintenance, PausedCount: interval.Counts.Paused,
	}
	if !interval.To.IsZero() {
		t := interval.To.UTC()
		row.EndedAt = &t
	}
	return row
}

func (m probeDirtyBucketModel) bucket() domain.DirtyBucket {
	return domain.DirtyBucket{MonitorID: m.MonitorID, ProbeID: m.ProbeID, Resolution: m.Resolution, Bucket: m.Bucket.UTC()}
}

// PutHealthState replaces the materialized current overall snapshot.
func (r *RegionalCommitStore) PutHealthState(ctx context.Context, state *domain.MonitorHealthState) error {
	if state == nil || state.MonitorID <= 0 || state.ProjectionVersion < 1 ||
		(state.Policy != domain.HealthPolicyAnyDown && state.Policy != domain.HealthPolicyAllDown) {
		return fmt.Errorf("health state: %w", domain.ErrValidation)
	}
	now := time.Now().UTC()
	row := healthStateModel(*state, now)
	existing := new(monitorHealthStateModel)
	err := r.db.NewSelect().Model(existing).Where("monitor_id = ?", state.MonitorID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = r.db.NewInsert().Model(&row).Exec(ctx)
		return err
	}
	if err != nil {
		return fmt.Errorf("get health state: %w", err)
	}
	_, err = r.db.NewUpdate().Model(&row).Where("monitor_id = ?", state.MonitorID).Exec(ctx)
	return err
}

// GetHealthState returns the materialized current overall snapshot.
func (r *RegionalCommitStore) GetHealthState(ctx context.Context, monitorID int64) (*domain.MonitorHealthState, error) {
	m := new(monitorHealthStateModel)
	if err := r.db.NewSelect().Model(m).Where("monitor_id = ?", monitorID).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get health state: %w", probeRegistryError(err))
	}
	state := m.state()
	return &state, nil
}

// ReplaceHealthHistory atomically replaces overall intervals overlapping [from, to).
func (r *RegionalCommitStore) ReplaceHealthHistory(ctx context.Context, monitorID int64, from, to time.Time, intervals []domain.MonitorHealthInterval) error {
	from, to = from.UTC(), to.UTC()
	if monitorID <= 0 || !from.Before(to) {
		return fmt.Errorf("health history window: %w", domain.ErrValidation)
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewUpdate().Table("monitor_health_history").
			Set("ended_at = ?", from).
			Where("monitor_id = ? AND started_at < ? AND (ended_at IS NULL OR ended_at > ?)", monitorID, from, from).
			Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewDelete().Table("monitor_health_history").
			Where("monitor_id = ? AND started_at >= ? AND started_at < ?", monitorID, from, to).
			Exec(ctx); err != nil {
			return err
		}
		for _, interval := range intervals {
			clipped := interval
			if clipped.From.Before(from) {
				clipped.From = from
			}
			if clipped.To.IsZero() || clipped.To.After(to) {
				clipped.To = to
			}
			if !clipped.From.Before(clipped.To) {
				continue
			}
			if clipped.Cause == "" {
				clipped.Cause = domain.HealthHistoryCauseAdministrative
			}
			row := healthHistoryModel(monitorID, 0, clipped)
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListHealthHistory returns overall intervals overlapping [from, to), clipped to the window.
func (r *RegionalCommitStore) ListHealthHistory(ctx context.Context, monitorID int64, from, to time.Time) ([]domain.MonitorHealthInterval, error) {
	from, to = from.UTC(), to.UTC()
	var rows []monitorHealthHistoryModel
	if err := r.db.NewSelect().Model(&rows).
		Where("monitor_id = ?", monitorID).
		Where("started_at < ?", to).
		Where("ended_at IS NULL OR ended_at > ?", from).
		Order("started_at ASC", "id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list health history: %w", err)
	}
	out := make([]domain.MonitorHealthInterval, 0, len(rows))
	for _, row := range rows {
		interval := row.interval()
		if interval.From.Before(from) {
			interval.From = from
		}
		if interval.To.IsZero() || interval.To.After(to) {
			interval.To = to
		}
		if interval.From.Before(interval.To) {
			out = append(out, interval)
		}
	}
	return out, nil
}

// MarkDirty records late-data recomputation work. Duplicate keys are no-ops.
func (r *RegionalCommitStore) MarkDirty(ctx context.Context, buckets []domain.DirtyBucket) error {
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return markDirtyTx(ctx, tx, buckets)
	})
}

// ListDirty returns durable dirty buckets for one resolution, oldest bucket first.
func (r *RegionalCommitStore) ListDirty(ctx context.Context, resolution string, limit int) ([]domain.DirtyBucket, error) {
	if resolution == "" {
		return nil, fmt.Errorf("dirty resolution: %w", domain.ErrValidation)
	}
	var rows []probeDirtyBucketModel
	q := r.db.NewSelect().Model(&rows).
		Where("resolution = ?", resolution).
		Order("bucket ASC", "monitor_id ASC", "probe_id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("list dirty buckets: %w", err)
	}
	out := make([]domain.DirtyBucket, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.bucket())
	}
	return out, nil
}

// ClearDirty removes processed dirty-bucket markers.
func (r *RegionalCommitStore) ClearDirty(ctx context.Context, buckets []domain.DirtyBucket) error {
	for _, bucket := range buckets {
		if _, err := r.db.NewDelete().Model((*probeDirtyBucketModel)(nil)).
			Where("monitor_id = ? AND probe_id = ? AND resolution = ? AND bucket = ?",
				bucket.MonitorID, bucket.ProbeID, bucket.Resolution, bucket.Bucket.UTC()).
			Exec(ctx); err != nil {
			return fmt.Errorf("clear dirty bucket: %w", err)
		}
	}
	return nil
}

func markDirtyTx(ctx context.Context, tx bun.Tx, buckets []domain.DirtyBucket) error {
	for _, bucket := range buckets {
		if bucket.MonitorID <= 0 || bucket.ProbeID == "" || bucket.Resolution == "" || bucket.Bucket.IsZero() {
			return fmt.Errorf("dirty bucket: %w", domain.ErrValidation)
		}
		row := probeDirtyBucketModel{
			MonitorID: bucket.MonitorID, ProbeID: bucket.ProbeID,
			Resolution: bucket.Resolution, Bucket: bucket.Bucket.UTC(),
		}
		exists, err := tx.NewSelect().Model((*probeDirtyBucketModel)(nil)).
			Where("monitor_id = ? AND probe_id = ? AND resolution = ? AND bucket = ?",
				row.MonitorID, row.ProbeID, row.Resolution, row.Bucket).
			Exists(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			if errors.Is(probeRegistryError(err), ports.ErrConflict) {
				continue
			}
			return err
		}
	}
	return nil
}
