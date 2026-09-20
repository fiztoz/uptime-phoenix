package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeHistoryWorkRepository = (*RegionalCommitStore)(nil)

const maxHistoryEvidence = 100000

// ReadHistoryEvidence loads the same sequence and gap evidence used by the
// background worker. Authorization belongs to the calling core service.
func (r *RegionalCommitStore) ReadHistoryEvidence(ctx context.Context, monitorID int64, from, to time.Time, projector ports.ProbeHistoryProjector) (domain.OverallHistoryInput, error) {
	var result domain.OverallHistoryInput
	if monitorID <= 0 || from.IsZero() || !from.Before(to) || projector == nil {
		return result, domain.ErrValidation
	}
	err := r.db.RunInTx(ctx, historyTxOptions(r.db), func(ctx context.Context, tx bun.Tx) error {
		var err error
		result, err = readHistoryEvidenceWindow(ctx, tx, monitorID, from.UTC(), to.UTC(), domain.DirtyResolutionOverall, projector)
		return err
	})
	return result, err
}

var errHistoryChanged = errors.New("historical evidence changed during projection")

// ProcessHistoryWork consumes bounded closed windows and resumes gap expansion.
// Re-marking creates a fresh source revision. An optimistic coherent read can only
// publish after a current row lock proves that revision is still unchanged.
func (r *RegionalCommitStore) ProcessHistoryWork(ctx context.Context, now time.Time, limit int, projector ports.ProbeHistoryProjector) (int, error) {
	if now.IsZero() || limit <= 0 || limit > 1000 || projector == nil {
		return 0, domain.ErrValidation
	}
	now = now.UTC()
	processed := 0
	for processed < limit {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		// Alternate bounded range expansion with projection so a long gap cannot
		// fill memory or starve already queued historical buckets.
		if processed == 0 || processed%8 == 0 {
			expanded, err := r.expandHistoryRanges(ctx, now, projector)
			if err != nil {
				return processed, err
			}
			if expanded {
				processed++
				if processed == limit {
					break
				}
			}
		}
		var row probeDirtyBucketModel
		err := r.db.NewSelect().Model(&row).Where("(resolution IN ('1m','overall') AND bucket < ?) OR (resolution = '1h' AND bucket < ?) OR (resolution = '1d' AND bucket < ?)", now.Truncate(time.Minute), now.Truncate(time.Hour), now.Truncate(24*time.Hour)).OrderExpr("CASE resolution WHEN '1m' THEN 0 WHEN 'overall' THEN 1 WHEN '1h' THEN 2 ELSE 3 END").Order("bucket ASC", "monitor_id ASC", "probe_id ASC").Limit(1).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return processed, nil
		}
		if err != nil {
			return processed, err
		}
		if row.Resolution == domain.DirtyResolution1h || row.Resolution == domain.DirtyResolution1d {
			expanded, err := r.expandHistoryRanges(ctx, now, projector)
			if err != nil {
				return processed, err
			}
			if expanded {
				processed++
				continue
			}
		}
		if err := r.recomputeHistoryWindow(ctx, row, projector); historyRevisionChanged(err) {
			return processed, nil
		} else if err != nil {
			return processed, err
		}
		processed++
	}
	return processed, nil
}

func historyRevisionChanged(err error) bool {
	if errors.Is(err, errHistoryChanged) {
		return true
	}
	var conflict *mysql.MySQLError
	// MariaDB can reject the current locking read itself after a concurrent
	// source commit, before the explicit token comparison. RunInTx has already
	// rolled back; leave the work for a fresh coherent read on the next batch.
	return errors.As(err, &conflict) && (conflict.Number == 1020 || conflict.Number == 1213)
}

func historyTxOptions(db *bun.DB) *sql.TxOptions {
	if db.Dialect().Name() == dialect.MySQL {
		return &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}
	return nil
}

func dirtyIdentity(q *bun.SelectQuery, b probeDirtyBucketModel) *bun.SelectQuery {
	return q.Where("monitor_id = ? AND probe_id = ? AND resolution = ? AND bucket = ?", b.MonitorID, b.ProbeID, b.Resolution, b.Bucket.UTC())
}

func (r *RegionalCommitStore) recomputeHistoryWindow(ctx context.Context, candidate probeDirtyBucketModel, projector ports.ProbeHistoryProjector) error {
	return r.db.RunInTx(ctx, historyTxOptions(r.db), func(ctx context.Context, tx bun.Tx) error {
		if tx.Dialect().Name() != dialect.MySQL {
			// SQLite needs its writer lock before the coherent read, otherwise a
			// competing commit would make read-to-write promotion fail as SQLITE_BUSY.
			if _, err := tx.ExecContext(ctx, "UPDATE probe_dirty_buckets SET revision = revision WHERE monitor_id = ? AND probe_id = ? AND resolution = ? AND bucket = ?", candidate.MonitorID, candidate.ProbeID, candidate.Resolution, candidate.Bucket.UTC()); err != nil {
				return err
			}
		}
		var row probeDirtyBucketModel
		if err := dirtyIdentity(tx.NewSelect().Model(&row), candidate).Scan(ctx); errors.Is(err, sql.ErrNoRows) {
			return errHistoryChanged
		} else if err != nil {
			return err
		}
		// Queue ordering is only a hint: a source can re-mark a child between
		// selection and this transaction. Read dependencies in the same snapshot
		// as the token; later source commits are caught by the locked token check.
		if err := requireCleanHistoryChildren(ctx, tx, row); err != nil {
			return err
		}
		evidence, err := loadHistoryEvidence(ctx, tx, row, projector)
		if err != nil {
			return err
		}
		children, err := readHistoryChildren(ctx, tx, row)
		if err != nil {
			return err
		}
		result, err := projector.ProjectHistory(ctx, domain.ProbeHistoryWork{Bucket: row.bucket(), Evidence: evidence, Children: children})
		if err != nil {
			return err
		}
		var current probeDirtyBucketModel
		q := dirtyIdentity(tx.NewSelect().Model(&current), row)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); errors.Is(err, sql.ErrNoRows) {
			return errHistoryChanged
		} else if err != nil {
			return err
		}
		if current.Revision != row.Revision {
			return errHistoryChanged
		}
		if row.Resolution == domain.DirtyResolutionOverall {
			if result.Regional != nil {
				return domain.ErrValidation
			}
			if err := replaceHistoryWindowTx(ctx, tx, row.MonitorID, evidence.From, evidence.To, result.Overall); err != nil {
				return err
			}
		} else {
			if result.Regional == nil || len(result.Overall) != 0 {
				return domain.ErrValidation
			}
			if err := saveHistoryAggregate(ctx, tx, row, *result.Regional); err != nil {
				return err
			}
		}
		if row.Resolution == domain.DirtyResolution1m {
			// Carry-forward/freshness also changes subsequent minutes, even when no
			// next check arrives. Each step is durable and bounded by one minute.
			for _, obs := range evidence.Observations {
				if obs.ProbeID == row.ProbeID && obs.ObservedAt.Add(evidence.FreshFor).After(evidence.To) {
					next := domain.RegionalObservation{MonitorID: row.MonitorID, ProbeID: row.ProbeID, ObservedAt: evidence.To}
					if err := markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(next)); err != nil {
						return err
					}
					break
				}
			}
		}
		// Last write: a fault here must roll back aggregate/history replacement too.
		_, err = tx.NewDelete().Model((*probeDirtyBucketModel)(nil)).Where("monitor_id = ? AND probe_id = ? AND resolution = ? AND bucket = ? AND revision = ?", row.MonitorID, row.ProbeID, row.Resolution, row.Bucket.UTC(), row.Revision).Exec(ctx)
		return err
	})
}

func requireCleanHistoryChildren(ctx context.Context, tx bun.Tx, row probeDirtyBucketModel) error {
	var resolutions []string
	switch row.Resolution {
	case domain.DirtyResolution1h:
		resolutions = []string{domain.DirtyResolution1m}
	case domain.DirtyResolution1d:
		resolutions = []string{domain.DirtyResolution1m, domain.DirtyResolution1h}
	default:
		return nil
	}
	dirty, err := tx.NewSelect().Model((*probeDirtyBucketModel)(nil)).
		Where("monitor_id = ? AND probe_id = ? AND bucket >= ? AND bucket < ?", row.MonitorID, row.ProbeID, row.Bucket.UTC(), row.Bucket.UTC().Add(historyBucketWidth(row.Resolution))).
		Where("resolution IN (?)", bun.List(resolutions)).Exists(ctx)
	if err != nil {
		return err
	}
	if dirty {
		return errHistoryChanged
	}
	return nil
}

func historyBucketWidth(resolution string) time.Duration {
	switch resolution {
	case domain.DirtyResolution1m, domain.DirtyResolutionOverall:
		return time.Minute
	case domain.DirtyResolution1h:
		return time.Hour
	case domain.DirtyResolution1d:
		return 24 * time.Hour
	default:
		return 0
	}
}

func loadHistoryEvidence(ctx context.Context, tx bun.Tx, row probeDirtyBucketModel, projector ports.ProbeHistoryProjector) (domain.OverallHistoryInput, error) {
	return readHistoryEvidenceWindow(ctx, tx, row.MonitorID, row.Bucket.UTC(), row.Bucket.UTC().Add(historyBucketWidth(row.Resolution)), row.Resolution, projector)
}

func readHistoryEvidenceWindow(ctx context.Context, tx bun.Tx, monitorID int64, from, to time.Time, resolution string, projector ports.ProbeHistoryProjector) (domain.OverallHistoryInput, error) {
	in := domain.OverallHistoryInput{MonitorID: monitorID, From: from, To: to}
	var monitor MonitorModel
	if err := tx.NewSelect().Model(&monitor).Where("id = ?", monitorID).Scan(ctx); err != nil {
		return in, err
	}
	fresh, err := projector.HistoryFreshness(*monitor.ToDomain())
	if err != nil {
		return in, err
	}
	in.FreshFor = fresh
	in.Paused = !monitor.Active
	var assignments []probeAssignmentHistoryModel
	if err := tx.NewSelect().Model(&assignments).Where("monitor_id = ? AND started_at < ? AND (ended_at IS NULL OR ended_at > ?)", monitorID, in.To, in.From).Order("started_at ASC", "id ASC").Limit(10001).Scan(ctx); err != nil {
		return in, err
	}
	if len(assignments) > 10000 {
		return in, domain.ErrValidation
	}
	for _, a := range assignments {
		interval := domain.AssignmentInterval{ProbeID: a.ProbeID, Generation: a.Generation, Revision: a.Revision, Policy: a.HealthPolicy, From: a.StartedAt.UTC()}
		if a.EndedAt != nil {
			interval.To = a.EndedAt.UTC()
		}
		in.Assignments = append(in.Assignments, interval)
	}
	if resolution == domain.DirtyResolution1h || resolution == domain.DirtyResolution1d {
		return in, nil
	}
	if len(assignments) == 0 {
		return in, nil
	}
	probeSet, generationSet := make(map[string]bool), make(map[int64]bool)
	for _, a := range assignments {
		probeSet[a.ProbeID] = true
		generationSet[a.Generation] = true
	}
	probeIDs := make([]string, 0, len(probeSet))
	generations := make([]int64, 0, len(generationSet))
	for id := range probeSet {
		probeIDs = append(probeIDs, id)
	}
	for generation := range generationSet {
		generations = append(generations, generation)
	}
	slices.Sort(probeIDs)
	slices.Sort(generations)
	var observations []probeObservationModel
	lookback := in.From.Add(-fresh)
	if err := tx.NewSelect().Model(&observations).Where("monitor_id = ? AND observed_at >= ? AND observed_at < ?", monitorID, lookback, in.To).Where("probe_id IN (?) AND assignment_generation IN (?)", bun.List(probeIDs), bun.List(generations)).Order("observed_at ASC", "id ASC").Limit(maxHistoryEvidence + 1).Scan(ctx); err != nil {
		return in, err
	}
	if len(observations) > maxHistoryEvidence {
		return in, domain.ErrValidation
	}
	// Retain the sequence-leading old sample as a seed. After a backward clock,
	// an older sequence with a later timestamp must not regain authority merely
	// because the actual later sequence has become stale outside the lookback.
	var seeds []probeObservationModel
	query := `SELECT o.* FROM probe_observations AS o JOIN
 (SELECT id, ROW_NUMBER() OVER (PARTITION BY probe_id,assignment_generation,stream_id ORDER BY seq DESC,id DESC) AS position
 FROM probe_observations WHERE monitor_id = ? AND observed_at < ? AND probe_id IN (?) AND assignment_generation IN (?)) AS ranked ON ranked.id = o.id
 WHERE ranked.position = 1 LIMIT 10001`
	if err := tx.NewRaw(query, monitorID, lookback, bun.List(probeIDs), bun.List(generations)).Scan(ctx, &seeds); err != nil {
		return in, err
	}
	if len(seeds) > 10000 {
		return in, domain.ErrValidation
	}
	for _, o := range append(seeds, observations...) {
		in.Observations = append(in.Observations, o.observation())
	}
	in.Gaps, err = readHistoryGaps(ctx, tx, monitorID, in.To)
	return in, err
}

func readHistoryChildren(ctx context.Context, tx bun.Tx, row probeDirtyBucketModel) ([]domain.RegionalHistoryRollup, error) {
	resolution := ""
	limit := 0
	switch row.Resolution {
	case domain.DirtyResolution1h:
		resolution = domain.DirtyResolution1m
		limit = 60
	case domain.DirtyResolution1d:
		resolution = domain.DirtyResolution1h
		limit = 24
	default:
		return nil, nil
	}
	var children []AggregateModel
	if err := tx.NewSelect().Model(&children).ModelTableExpr("heartbeat_"+resolution+" AS aggregate_model").Where("monitor_id = ? AND probe_id = ? AND bucket >= ? AND bucket < ?", row.MonitorID, row.ProbeID, row.Bucket.UTC(), row.Bucket.UTC().Add(historyBucketWidth(row.Resolution))).Order("bucket ASC", "id ASC").Limit(limit + 1).Scan(ctx); err != nil {
		return nil, err
	}
	if len(children) > limit {
		return nil, domain.ErrValidation
	}
	out := make([]domain.RegionalHistoryRollup, 0, len(children))
	for _, child := range children {
		out = append(out, domain.RegionalHistoryRollup{MonitorID: child.MonitorID, ProbeID: child.ProbeID, Resolution: resolution, Bucket: child.Bucket.UTC(), UpCount: child.UpCount, DownCount: child.DownCount, PendingCount: child.PendingCount, MaintCount: child.MaintCount, UnknownCount: child.UnknownCount, TotalChecks: child.TotalChecks, PingCount: child.PingCount, AvgPing: child.AvgPing, MinPing: derefInt(child.MinPing), MaxPing: derefInt(child.MaxPing), Durations: child.durations()})
	}
	return out, nil
}

func readHistoryGaps(ctx context.Context, db bun.IDB, monitorID int64, to time.Time) ([]domain.RegionalHistoryGap, error) {
	var rows []replayGapModel
	if err := db.NewSelect().Model(&rows).Where("observed_from < ?", to.UTC()).Where("probe_id IN (SELECT probe_id FROM monitor_probe_assignment_history WHERE monitor_id = ?)", monitorID).Order("observed_from ASC", "probe_id ASC", "stream_id ASC", "from_seq ASC").Limit(10001).Scan(ctx); err != nil {
		return nil, err
	}
	if len(rows) > 10000 {
		return nil, domain.ErrValidation
	}
	out := make([]domain.RegionalHistoryGap, 0, len(rows))
	for _, row := range rows {
		var ids []int64
		if err := json.Unmarshal([]byte(row.AffectedMonitorIDs), &ids); err != nil {
			return nil, err
		}
		if len(ids) > 0 && !slices.Contains(ids, monitorID) {
			continue
		}
		out = append(out, domain.RegionalHistoryGap{ProbeID: row.ProbeID, StreamID: row.StreamID, FromSeq: row.FromSeq, ThroughSeq: row.ThroughSeq, From: row.ObservedFrom.UTC(), Through: row.ObservedThrough.UTC()})
	}
	return out, nil
}

func saveHistoryAggregate(ctx context.Context, tx bun.Tx, bucket probeDirtyBucketModel, a domain.RegionalHistoryRollup) error {
	if a.MonitorID != bucket.MonitorID || a.ProbeID != bucket.ProbeID || a.Resolution != bucket.Resolution || !a.Bucket.Equal(bucket.Bucket) {
		return domain.ErrValidation
	}
	table := "heartbeat_" + bucket.Resolution
	if historyBucketWidth(bucket.Resolution) == 0 || bucket.Resolution == domain.DirtyResolutionOverall {
		return domain.ErrValidation
	}
	m := AggregateModel{MonitorID: a.MonitorID, ProbeID: a.ProbeID, Bucket: a.Bucket.UTC(), UpCount: a.UpCount, DownCount: a.DownCount, PendingCount: a.PendingCount, MaintCount: a.MaintCount, UnknownCount: a.UnknownCount, TotalChecks: a.TotalChecks, PingCount: a.PingCount, AvgPing: a.AvgPing, MinPing: &a.MinPing, MaxPing: &a.MaxPing, HistoryManaged: true, UpUS: a.Durations.Up.Microseconds(), DownUS: a.Durations.Down.Microseconds(), PendingUS: a.Durations.Pending.Microseconds(), UnknownUS: a.Durations.Unknown.Microseconds(), MaintenanceUS: a.Durations.Maintenance.Microseconds(), PausedUS: a.Durations.Paused.Microseconds()}
	q := tx.NewInsert().Model(&m).ModelTableExpr(table)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.On("DUPLICATE KEY UPDATE")
	} else {
		q = q.On(AggregateConflictTarget)
	}
	for _, column := range []string{"up_count", "down_count", "pending_count", "maint_count", "unknown_count", "total_checks", "ping_count", "avg_ping", "min_ping", "max_ping", "history_managed", "up_us", "down_us", "pending_us", "unknown_us", "maintenance_us", "paused_us"} {
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.Set("? = VALUES(?)", bun.Ident(column), bun.Ident(column))
		} else {
			q = q.Set("? = EXCLUDED.?", bun.Ident(column), bun.Ident(column))
		}
	}
	_, err := q.Exec(ctx)
	return err
}

func replaceHistoryWindowTx(ctx context.Context, tx bun.Tx, monitorID int64, from, to time.Time, intervals []domain.MonitorHealthInterval) error {
	var overlaps []monitorHealthHistoryModel
	query := tx.NewSelect().Model(&overlaps).Where("monitor_id = ? AND started_at < ? AND (ended_at IS NULL OR ended_at > ?)", monitorID, to, from)
	if tx.Dialect().Name() == dialect.MySQL {
		query = query.For("UPDATE")
	}
	if err := query.Scan(ctx); err != nil {
		return err
	}
	for _, row := range overlaps {
		if _, err := tx.NewDelete().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		if row.StartedAt.Before(from) {
			left := row
			left.ID = 0
			left.EndedAt = &from
			if _, err := tx.NewInsert().Model(&left).Exec(ctx); err != nil {
				return err
			}
		}
		if row.EndedAt == nil || row.EndedAt.After(to) {
			right := row
			right.ID = 0
			right.StartedAt = to
			if _, err := tx.NewInsert().Model(&right).Exec(ctx); err != nil {
				return err
			}
		}
	}
	for _, interval := range intervals {
		if interval.From.Before(from) || interval.To.After(to) || !interval.From.Before(interval.To) {
			return domain.ErrValidation
		}
		row := healthHistoryModel(monitorID, 0, interval)
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (r *RegionalCommitStore) expandHistoryGap(ctx context.Context, now time.Time, projector ports.ProbeHistoryProjector) (bool, error) {
	var candidate replayGapModel
	err := r.db.NewSelect().Model(&candidate).Where("recompute_pending = ? AND recompute_bucket < ?", true, now.Truncate(time.Minute)).Order("observed_from ASC", "probe_id ASC", "stream_id ASC", "from_seq ASC").Limit(1).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	changed := false
	err = r.db.RunInTx(ctx, historyTxOptions(r.db), func(ctx context.Context, tx bun.Tx) error {
		if tx.Dialect().Name() != dialect.MySQL {
			if _, err := tx.ExecContext(ctx, "UPDATE probe_telemetry_gaps SET recompute_pending = recompute_pending WHERE probe_id = ? AND stream_id = ? AND from_seq = ?", candidate.ProbeID, candidate.StreamID, candidate.FromSeq); err != nil {
				return err
			}
		}
		var gap replayGapModel
		q := tx.NewSelect().Model(&gap).Where("probe_id = ? AND stream_id = ? AND from_seq = ?", candidate.ProbeID, candidate.StreamID, candidate.FromSeq)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); err != nil {
			return err
		}
		if !gap.RecomputePending {
			return nil
		}
		var ids []int64
		if err := json.Unmarshal([]byte(gap.AffectedMonitorIDs), &ids); err != nil {
			return err
		}
		var monitor MonitorModel
		mq := tx.NewSelect().Model(&monitor).Where("id >= ?", gap.RecomputeMonitorID).Where("id IN (SELECT monitor_id FROM monitor_probe_assignment_history WHERE probe_id = ?)", gap.ProbeID).Order("id ASC").Limit(1)
		if len(ids) > 0 {
			mq = mq.Where("id IN (?)", bun.List(ids))
		}
		err := mq.Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			gap.RecomputePending = false
		} else if err != nil {
			return err
		} else {
			if gap.RecomputeMonitorID != monitor.ID {
				gap.RecomputeMonitorID = monitor.ID
				gap.RecomputeBucket = gap.ObservedFrom.UTC().Truncate(time.Minute)
			}
			fresh, err := projector.HistoryFreshness(*monitor.ToDomain())
			if err != nil {
				return err
			}
			through := gap.ObservedThrough.UTC().Add(fresh).Truncate(time.Minute)
			if !gap.RecomputeBucket.Before(now.Truncate(time.Minute)) {
				return nil
			}
			if err := markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(domain.RegionalObservation{MonitorID: monitor.ID, ProbeID: gap.ProbeID, ObservedAt: gap.RecomputeBucket.UTC()})); err != nil {
				return err
			}
			gap.RecomputeBucket = gap.RecomputeBucket.UTC().Add(time.Minute)
			if gap.RecomputeBucket.After(through) {
				gap.RecomputeMonitorID = monitor.ID + 1
				gap.RecomputeBucket = gap.ObservedFrom.UTC().Truncate(time.Minute)
			}
		}
		if _, err := tx.NewUpdate().Model(&gap).Column("recompute_pending", "recompute_monitor_id", "recompute_bucket").WherePK().Exec(ctx); err != nil {
			return fmt.Errorf("advance gap recomputation: %w", err)
		}
		changed = true
		return nil
	})
	return changed, err
}
