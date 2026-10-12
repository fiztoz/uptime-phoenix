package repository

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.RemoteProbeDeliveryConfigReader = (*RemoteProbeConfigSyncStore)(nil)

// ReadAcknowledgedRemoteDelivery projects only nonsecret facts from the exact
// authenticated configuration document. No confidential graph crosses this port.
func (s *RemoteProbeConfigSyncStore) ReadAcknowledgedRemoteDelivery(ctx context.Context, target domain.ProbeConfigTarget) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	config, err := s.ReadAppliedRemote(ctx, target)
	if err != nil {
		return nil, err
	}
	result := &domain.AcknowledgedProbeDeliveryConfig{Target: config.Metadata.ProbeConfigTarget, Revision: config.Metadata.Revision,
		Assignments: make([]domain.AcknowledgedAssignmentDelivery, 0, len(config.Assignments))}
	seen := make(map[int64]bool, len(config.Assignments))
	for _, a := range config.Assignments {
		mode, valid := domain.CanonicalAlertDelivery(a.DesiredAlertDelivery)
		if a.Monitor == nil || a.Monitor.ID <= 0 || a.Generation <= 0 || a.AssignmentRevision < 0 || !valid || seen[a.Monitor.ID] {
			return nil, ports.ErrConflict
		}
		seen[a.Monitor.ID] = true
		result.Assignments = append(result.Assignments, domain.AcknowledgedAssignmentDelivery{MonitorID: a.Monitor.ID, Generation: a.Generation, AssignmentRevision: a.AssignmentRevision, AlertDelivery: mode})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
