package repository

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// LatestObservations selects the persisted sample named by each active current
// state. Reconnect snapshots whose history has not arrived produce no fake row.
func (r *RegionalCommitStore) LatestObservations(ctx context.Context, monitorID int64) ([]domain.RegionalObservation, error) {
	var rows []probeObservationModel
	err := r.db.NewSelect().Model(&rows).
		Join("JOIN monitor_probe_state s ON s.monitor_id = obs.monitor_id AND s.probe_id = obs.probe_id AND s.stream_id = obs.stream_id AND s.seq = obs.seq").
		Join("JOIN monitor_probe_assignments a ON a.monitor_id = s.monitor_id AND a.probe_id = s.probe_id AND a.generation = s.assignment_generation AND a.active = ?", true).
		Where("obs.monitor_id = ?", monitorID).OrderExpr("obs.observed_at ASC, obs.id ASC").Scan(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RegionalObservation, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.observation())
	}
	return out, nil
}
