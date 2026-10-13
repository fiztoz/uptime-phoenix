package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// localPreparationsComplete checks refresh completeness only. Missing state
// upgrades by new publication; present inconsistent state never repairs itself.
func (s *LocalProbeConfigRefreshService) localPreparationsComplete(ctx context.Context, active domain.ProbeActiveConfig, definition *domain.LocalProbeConfigDefinition) (bool, error) {
	if s.localPreparations == nil {
		return true, nil
	} // Unconfigured legacy caller, no preparation claim.
	preparations, err := s.localPreparations.ReadLocalDeliveryPreparations(ctx, active)
	if err != nil {
		return false, err
	}
	return completeLocalDeliveryPreparations(active, definition, preparations)
}

func completeLocalDeliveryPreparations(active domain.ProbeActiveConfig, definition *domain.LocalProbeConfigDefinition, preparations []domain.LocalDeliveryPreparation) (bool, error) {
	if definition == nil || definition.Target != active.ProbeConfigTarget || definition.Revision != active.Revision {
		return false, ports.ErrConflict
	}
	eligible := make(map[int64]domain.ProbeConfigAssignment)
	for _, member := range definition.Assignments {
		if member.Monitor == nil {
			return false, ports.ErrConflict
		}
		mode, ok := domain.CanonicalAlertDelivery(member.AlertDelivery)
		if !ok {
			return false, ports.ErrConflict
		}
		if mode == domain.AlertDeliveryAggregate || mode == domain.AlertDeliveryBoth {
			if member.AssignmentRevision <= 0 || member.Generation <= 0 {
				return false, ports.ErrConflict
			}
			if _, duplicate := eligible[member.Monitor.ID]; duplicate {
				return false, ports.ErrConflict
			}
			eligible[member.Monitor.ID] = member
		}
	}
	seen := make(map[int64]struct{}, len(preparations))
	for _, p := range preparations {
		member, exists := eligible[p.MonitorID]
		if !exists || !domain.ValidLocalDeliveryPreparation(p) || p.ProbeConfigTarget != active.ProbeConfigTarget || p.ConfigRevision != active.Revision || p.SHA256 != active.SHA256 || p.AssignmentRevision != member.AssignmentRevision || p.Generation != member.Generation || p.AlertDelivery != member.AlertDelivery {
			return false, ports.ErrConflict
		}
		if _, duplicate := seen[p.MonitorID]; duplicate {
			return false, ports.ErrConflict
		}
		seen[p.MonitorID] = struct{}{}
	}
	return len(seen) == len(eligible), nil
}
