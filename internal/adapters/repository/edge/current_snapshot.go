package edge

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeStateRepository = (*Store)(nil)

// ReadCurrentSnapshot captures accepted assignments and exact current evidence
// in one SQLite read transaction. History retention and ACK pruning are unrelated.
func (s *Store) ReadCurrentSnapshot(ctx context.Context, fence domain.EdgeReplayFence, at time.Time) (domain.EdgeCurrentSnapshot, error) {
	var snapshot domain.EdgeCurrentSnapshot
	if !domain.ValidHubID(fence.HubID) || !domain.ValidHubID(fence.ProbeID) || !domain.ValidHubID(fence.StreamID) || fence.ConnectionGeneration <= 0 || at.IsZero() {
		return snapshot, domain.ErrValidation
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		i, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		if i.HubID != fence.HubID || i.ProbeID != fence.ProbeID || i.StreamID != fence.StreamID || i.ConnectionGeneration != fence.ConnectionGeneration {
			return ports.ErrConflict
		}
		if i.ConfigRevision <= 0 {
			return ports.ErrNotFound
		}
		// Bound allocations before fetching any payloads. This is the same read
		// snapshot as identity/config, so simultaneous checks cannot split it.
		from := ` FROM edge_regional_state AS s JOIN edge_assignments AS a ON a.monitor_id = s.monitor_id AND a.generation = s.generation AND a.revision = ? AND a.active = 1 WHERE length(s.current_observation) > 0`
		var limits struct {
			Count int
			Bytes int64
		}
		if err := tx.NewRaw("SELECT COUNT(*) AS count, COALESCE(SUM(length(s.current_observation)),0) AS bytes"+from, i.ConfigRevision).Scan(ctx, &limits); err != nil {
			return err
		}
		if limits.Count > 10000 || limits.Bytes > 16<<20 {
			return domain.ErrValidation
		}
		var rows []struct {
			MonitorID          int64
			Generation         int64
			Seq                int64
			CurrentObservation []byte
			SourceAlertID      *string
		}
		query := `SELECT s.monitor_id, s.generation, s.seq, s.current_observation,
		 (SELECT source_alert_id FROM edge_alerts WHERE source_alert_id = s.source_alert_id AND status IN ('firing','acked')) AS source_alert_id` + from + " ORDER BY s.monitor_id"
		if err := tx.NewRaw(query, i.ConfigRevision).Scan(ctx, &rows); err != nil {
			return err
		}
		snapshot = domain.EdgeCurrentSnapshot{Identity: i, CreatedAt: at.UTC(), States: make([]domain.EdgeCurrentEvidence, 0, len(rows))}
		for _, row := range rows {
			if row.Seq <= 0 || row.Seq > i.LastCreatedSeq {
				return ports.ErrConflict
			}
			snapshot.States = append(snapshot.States, domain.EdgeCurrentEvidence{MonitorID: row.MonitorID, AssignmentGeneration: row.Generation, Seq: row.Seq, Payload: row.CurrentObservation, ActiveSourceAlertID: row.SourceAlertID})
		}
		return nil
	})
	if err != nil {
		return domain.EdgeCurrentSnapshot{}, storageError(ctx, err)
	}
	return snapshot, nil
}
