package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// localAlertAssignmentActive prevents work from an obsolete assignment affecting
// the active lifecycle. The absent-set fallback is only local generation one.
func localAlertAssignmentActive(ctx context.Context, repo ports.MonitorProbeAssignmentRepository, monitorID int64, probeID string, generation int64) (bool, error) {
	if domain.NormalizeProbeID(probeID) != domain.LocalProbeID || generation < 0 {
		return false, nil
	}
	if generation == 0 {
		generation = 1
	}
	if repo == nil {
		return true, nil
	}
	set, err := repo.GetByMonitorID(ctx, monitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return generation == 1, nil
	}
	if err != nil {
		return false, err
	}
	if set == nil {
		return false, nil
	}
	for _, assignment := range set.Assignments {
		if assignment.ProbeID == domain.LocalProbeID && assignment.Generation == generation {
			return true, nil
		}
	}
	return false, nil
}
