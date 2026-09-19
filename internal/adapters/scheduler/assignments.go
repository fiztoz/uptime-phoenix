package scheduler

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// runnableMonitor couples an eligible monitor with its active local assignment generation.
type runnableMonitor struct {
	Monitor    *domain.Monitor
	Generation int64
}

func filterLocalRunnable(ctx context.Context, assignments ports.MonitorProbeAssignmentRepository, monitors []*domain.Monitor) ([]runnableMonitor, error) {
	if len(monitors) == 0 {
		return nil, nil
	}
	if assignments == nil {
		out := make([]runnableMonitor, len(monitors))
		for i, monitor := range monitors {
			out[i] = runnableMonitor{Monitor: monitor, Generation: 1}
		}
		return out, nil
	}
	ids := make([]int64, len(monitors))
	for i, monitor := range monitors {
		ids[i] = monitor.ID
	}
	allowed, err := assignments.ExecutableByLocal(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]runnableMonitor, 0, len(allowed))
	for _, monitor := range monitors {
		if gen, ok := allowed[monitor.ID]; ok {
			out = append(out, runnableMonitor{Monitor: monitor, Generation: gen})
		}
	}
	return out, nil
}
