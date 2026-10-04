package scheduler

import (
	"context"
	"fmt"

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

func captureScheduledChecks(ctx context.Context, runnable []runnableMonitor, activation ports.ProbeConfigActivationRepository, proxies *proxyResolver) ([]scheduledCheck, error) {
	var applied *domain.LocalProbeConfigDefinition
	if activation != nil {
		reader, ok := activation.(ports.LocalAppliedConfigReader)
		if !ok {
			return nil, fmt.Errorf("applied execution reader unavailable: %w", domain.ErrValidation)
		}
		var err error
		applied, err = reader.ReadAppliedLocal(ctx)
		if err != nil {
			return nil, err
		}
	}
	checks := make([]scheduledCheck, 0, len(runnable))
	for _, r := range runnable {
		m, gen, revision := r.Monitor, r.Generation, int64(0)
		var proxyConfig map[string]any
		if applied != nil {
			m = nil
			for _, a := range applied.Assignments {
				if a.Monitor.ID == r.Monitor.ID && a.Generation == gen {
					m = a.Monitor
					break
				}
			}
			if m == nil || !m.Active {
				continue
			}
			revision = applied.Revision
			if m.ProxyID != nil {
				for _, p := range applied.Proxies {
					if p.ID == *m.ProxyID {
						proxyConfig = proxyCheckConfig(p)
						break
					}
				}
			}
		} else {
			proxyConfig = proxies.configFor(ctx, m)
		}
		config := checkConfigForMonitor(m)
		config["_proxy"] = proxyConfig
		checks = append(checks, scheduledCheck{Monitor: m, Generation: gen, ConfigRevision: revision, CheckConfig: config})
	}
	return checks, nil
}
