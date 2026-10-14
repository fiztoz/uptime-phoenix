package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeDeliveryPreparationExecutor = (*EdgeConfigService)(nil)

// SetDeliveryPreparations enables durable preparation, never runtime application.
func (s *EdgeConfigService) SetDeliveryPreparations(repo ports.EdgeDeliveryPreparationRepository) {
	s.preparations = repo
}

// PrepareDelivery authenticates one captured config/fact tuple before committing
// a request-only preparation. Existing exact receipts recover under current
// accepted authority, not under a new inference about current mode eligibility.
func (s *EdgeConfigService) PrepareDelivery(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.ProbeDeliveryPreparation) (domain.ProbeCommandOutcome, error) {
	if s.preparations == nil || !domain.ValidProbeDeliveryPreparation(command) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	prior, err := s.preparations.ReadDeliveryPreparationReceipt(ctx, authority, command)
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	if prior != nil {
		return *prior, nil
	}
	active, err := s.store.ReadActiveConfig(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return s.preparations.PrepareDelivery(ctx, authority, command, nil)
	}
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	resolved, err := s.decodeActive(ctx, active)
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	// Desired mode is compared only as the preparation prerequisite. It never
	// supplies an applied-mode fact or changes the persisted effective mode.
	found := false
	for _, member := range resolved.Assignments {
		if member.Monitor != nil && member.Monitor.ID == command.MonitorID && member.Generation == command.Generation && member.AssignmentRevision == command.AssignmentRevision && member.DesiredAlertDelivery == command.AlertDelivery {
			found = true
			break
		}
	}
	if !found {
		return s.preparations.PrepareDelivery(ctx, authority, command, nil)
	}
	return s.preparations.PrepareDelivery(ctx, authority, command, &active)
}
