package repository

import (
	"context"
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type replayGapModel struct {
	bun.BaseModel      `bun:"table:probe_telemetry_gaps"`
	ProbeID            string `bun:",pk"`
	StreamID           string `bun:",pk"`
	FromSeq            int64  `bun:",pk"`
	ThroughSeq         int64
	Reason             string
	ObservedFrom       time.Time
	ObservedThrough    time.Time
	AffectedMonitorIDs string `bun:"affected_monitor_ids"`
	ReceivedAt         time.Time
	RecomputePending   bool
	RecomputeMonitorID int64
	RecomputeBucket    time.Time
}

// recordReplayGap runs only inside the same verified lease/stream transaction
// as normal replay. It adds loss metadata and durable range-recomputation work;
// previously committed evidence is never deleted or overwritten by a gap.
func recordReplayGap(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, gap *domain.ProbeTelemetryGap, cursor int64, now time.Time) error {
	if len(gap.AffectedMonitorIDs) > 0 {
		var count int
		if err := tx.NewSelect().Table("monitor_probe_assignment_history").ColumnExpr("COUNT(DISTINCT monitor_id)").Where("probe_id = ? AND monitor_id IN (?)", session.ProbeID, bun.List(gap.AffectedMonitorIDs)).Scan(ctx, &count); err != nil {
			return err
		}
		if count != len(gap.AffectedMonitorIDs) {
			return ports.ErrConflict
		}
	}
	if gap.ThroughSeq <= cursor {
		return nil
	}
	ids := gap.AffectedMonitorIDs
	if ids == nil {
		ids = []int64{}
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return domain.ErrValidation
	}
	row := replayGapModel{ProbeID: session.ProbeID, StreamID: session.StreamID, FromSeq: max(gap.FromSeq, cursor+1), ThroughSeq: gap.ThroughSeq, Reason: gap.Reason, ObservedFrom: gap.ObservedFrom.UTC(), ObservedThrough: gap.ObservedThrough.UTC(), AffectedMonitorIDs: string(encoded), ReceivedAt: now.UTC(), RecomputePending: true, RecomputeBucket: gap.ObservedFrom.UTC().Truncate(time.Minute)}
	_, err = tx.NewInsert().Model(&row).Exec(ctx)
	return err
}

func sequenceHasGap(ctx context.Context, tx bun.Tx, probeID, streamID string, seq int64) (bool, error) {
	return tx.NewSelect().Model((*replayGapModel)(nil)).Where("probe_id = ? AND stream_id = ? AND from_seq <= ? AND through_seq >= ?", probeID, streamID, seq, seq).Exists(ctx)
}
