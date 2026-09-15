package scheduler

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func filterLocalRunnable(ctx context.Context, assignments ports.MonitorProbeAssignmentRepository, monitors []*domain.Monitor) ([]*domain.Monitor, error) {
	if assignments == nil || len(monitors) == 0 {
		return monitors, nil
	}
	ids := make([]int64, len(monitors))
	for i, monitor := range monitors {
		ids[i] = monitor.ID
	}
	allowed, err := assignments.ExecutableByLocal(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Monitor, 0, len(allowed))
	for _, monitor := range monitors {
		if _, ok := allowed[monitor.ID]; ok {
			out = append(out, monitor)
		}
	}
	return out, nil
}
