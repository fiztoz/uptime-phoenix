package edge

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	queueRowBytes      = 256
	maxRetainedGaps    = 1024
	retentionSweepRows = 512
)

// RetentionPolicy bounds retained telemetry including conservative row overhead.
// Provider work has a separate budget. Zero policy keeps legacy fail-closed stores.
type RetentionPolicy struct {
	MaxBytes int64
	MaxAge   time.Duration
}

// WithRetentionPolicy enables atomic eviction with durable loss declarations.
// Configure before publishing the store; policies cannot change concurrently.
func WithRetentionPolicy(policy RetentionPolicy) Option {
	return func(s *Store) error {
		if policy.MaxBytes < 1<<20 || policy.MaxBytes > 1<<40 || policy.MaxAge < time.Hour || policy.MaxAge > 365*24*time.Hour {
			return domain.ErrValidation
		}
		s.retention = policy
		return nil
	}
}

type retainedGap struct {
	bun.BaseModel   `bun:"table:edge_gaps"`
	FromSeq         int64 `bun:",pk"`
	ThroughSeq      int64
	Reason          string
	ObservedFrom    int64
	ObservedThrough int64
}

func (g retainedGap) domain(stream string) *domain.ProbeTelemetryGap {
	return &domain.ProbeTelemetryGap{StreamID: stream, FromSeq: g.FromSeq, ThroughSeq: g.ThroughSeq, Reason: g.Reason, ObservedFrom: time.UnixMicro(g.ObservedFrom).UTC(), ObservedThrough: time.UnixMicro(g.ObservedThrough).UTC(), AffectedMonitorIDs: []int64{}}
}

func telemetryStorageBytes(ctx context.Context, tx bun.Tx) (int64, error) {
	var size int64
	err := tx.NewRaw("SELECT (SELECT COALESCE(SUM(length(payload) + ?),0) FROM edge_telemetry_outbox) + (SELECT COUNT(*) * ? FROM edge_gaps)", queueRowBytes, queueRowBytes).Scan(ctx, &size)
	return size, err
}

// SweepRetention expires a bounded chunk of old evidence. Recording invokes the
// same transaction helper; an idle probe uses this method so age retention runs
// even when checks are paused. Gaps commit before deleted evidence disappears.
func (s *Store) SweepRetention(ctx context.Context, now time.Time) error {
	if s.retention.MaxBytes == 0 {
		return nil
	}
	if now.IsZero() {
		return domain.ErrValidation
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if err := s.retainMetadata(ctx, tx, i, now); err != nil {
			return err
		}
		err := s.retainTelemetry(ctx, tx, now.UTC(), "", 0, 0)
		if errors.Is(err, ErrQueueFull) {
			// Keep this bounded sweep's durable progress. Source writes still
			// fail closed until subsequent sweeps restore the byte budget.
			return nil
		}
		return err
	})
}

func (s *Store) retainTelemetry(ctx context.Context, tx bun.Tx, now time.Time, kind string, incoming int64, reserved int64) error {
	if s.retention.MaxBytes == 0 {
		return nil
	}
	var old []struct {
		Seq        int64
		ObservedAt int64
	}
	if err := tx.NewRaw("SELECT seq, observed_at FROM edge_telemetry_outbox WHERE observed_at < ? ORDER BY observed_at, seq LIMIT ?", now.Add(-s.retention.MaxAge).UnixMicro(), retentionSweepRows).Scan(ctx, &old); err != nil {
		return err
	}
	for _, row := range old {
		if err := evictTelemetry(ctx, tx, row.Seq, row.ObservedAt, "retention_age"); err != nil {
			return err
		}
	}
	limit := s.retention.MaxBytes - reserved
	if kind == "observation" {
		// Ordinary samples leave at least ten percent for transitions, outcomes
		// and gap metadata, in addition to already leased provider outcomes.
		limit -= s.retention.MaxBytes / 10
	}
	for n := 0; n < retentionSweepRows; n++ {
		size, err := telemetryStorageBytes(ctx, tx)
		if err != nil {
			return err
		}
		if size <= limit-incoming {
			return nil
		}
		var row struct {
			Seq        int64
			ObservedAt int64
		}
		// Drop repetitive samples first; finite storage eventually drops older
		// critical events too, always under an explicit durable gap.
		err = tx.NewRaw("SELECT seq, observed_at FROM edge_telemetry_outbox ORDER BY CASE WHEN kind = 'observation' THEN 0 ELSE 1 END, seq LIMIT 1").Scan(ctx, &row)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrQueueFull
		}
		if err != nil {
			return err
		}
		if err := evictTelemetry(ctx, tx, row.Seq, row.ObservedAt, "retention_bytes"); err != nil {
			return err
		}
	}
	return ErrQueueFull
}

func evictTelemetry(ctx context.Context, tx bun.Tx, seq, observed int64, reason string) error {
	gap := retainedGap{FromSeq: seq, ThroughSeq: seq, Reason: reason, ObservedFrom: observed, ObservedThrough: observed}
	upper := seq
	if upper < math.MaxInt64 {
		upper++
	}
	var adjacent []retainedGap
	if err := tx.NewSelect().Model(&adjacent).Where("from_seq <= ? AND through_seq >= ?", upper, seq-1).Order("from_seq").Scan(ctx); err != nil {
		return err
	}
	for _, prior := range adjacent {
		gap.FromSeq = min(gap.FromSeq, prior.FromSeq)
		gap.ThroughSeq = max(gap.ThroughSeq, prior.ThroughSeq)
		gap.ObservedFrom = min(gap.ObservedFrom, prior.ObservedFrom)
		gap.ObservedThrough = max(gap.ObservedThrough, prior.ObservedThrough)
		if prior.Reason != gap.Reason {
			gap.Reason = "disk_pressure"
		}
		if _, err := tx.NewDelete().Model(&prior).WherePK().Exec(ctx); err != nil {
			return err
		}
	}
	if _, err := tx.NewInsert().Model(&gap).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq = ?", seq); err != nil {
		return err
	}
	var count int
	if err := tx.NewSelect().Table("edge_gaps").ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil {
		return err
	}
	if count <= maxRetainedGaps {
		return nil
	}
	// Bound fragmentation independently of outage duration. If alternating
	// transitions would exhaust metadata, declare the whole oldest span lost.
	var span retainedGap
	if err := tx.NewRaw("SELECT MIN(from_seq) AS from_seq, MAX(through_seq) AS through_seq, MIN(observed_from) AS observed_from, MAX(observed_through) AS observed_through FROM edge_gaps").Scan(ctx, &span); err != nil {
		return err
	}
	var bounds struct {
		First *int64
		Last  *int64
	}
	if err := tx.NewRaw("SELECT MIN(observed_at) AS first, MAX(observed_at) AS last FROM edge_telemetry_outbox WHERE seq BETWEEN ? AND ?", span.FromSeq, span.ThroughSeq).Scan(ctx, &bounds); err != nil {
		return err
	}
	if bounds.First != nil {
		span.ObservedFrom = min(span.ObservedFrom, *bounds.First)
		span.ObservedThrough = max(span.ObservedThrough, *bounds.Last)
	}
	span.Reason = "disk_pressure"
	if _, err := tx.ExecContext(ctx, "DELETE FROM edge_gaps"); err != nil {
		return err
	}
	if _, err := tx.NewInsert().Model(&span).Exec(ctx); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM edge_telemetry_outbox WHERE seq BETWEEN ? AND ?", span.FromSeq, span.ThroughSeq)
	return err
}
