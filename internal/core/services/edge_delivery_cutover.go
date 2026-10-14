package services

import (
	"context"
	"errors"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeDeliveryCutoverExecutor = (*EdgeConfigService)(nil)

// NewEdgeDeliveryCutoverExecutor resolves the durable source-gate repository
// from the service's existing repository dependency. The edge store implements
// both repositories, so the composition root needs no service struct change.
func NewEdgeDeliveryCutoverExecutor(s *EdgeConfigService) (ports.EdgeDeliveryCutoverExecutor, error) {
	if s == nil {
		return nil, domain.ErrValidation
	}
	if _, err := s.deliveryCutoverRepository(); err != nil {
		return nil, err
	}
	return s, nil
}

// deliveryCutoverRepository type-asserts the configured store. A store that
// cannot fence source gates and provider attempts must never execute cutovers.
func (s *EdgeConfigService) deliveryCutoverRepository() (ports.EdgeDeliveryCutoverRepository, error) {
	repo, ok := s.store.(ports.EdgeDeliveryCutoverRepository)
	if !ok || repo == nil {
		return nil, domain.ErrValidation
	}
	return repo, nil
}

// ExecuteDeliveryCutover authenticates one captured config/fact tuple before
// committing a protected source-gate effect. Exact historical receipts recover
// first under the accepted stored session; recovery is never new eligibility and
// never reloads current configuration. Resume releases only its stored barrier.
func (s *EdgeConfigService) ExecuteDeliveryCutover(ctx context.Context, authority domain.EdgeCommandAuthority, command domain.DeliveryCutoverCommand) (domain.ProbeCommandOutcome, error) {
	if !domain.ValidDeliveryCutoverCommand(command) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	repo, err := s.deliveryCutoverRepository()
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	prior, err := repo.ReadDeliveryCutoverReceipt(ctx, authority, command)
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	if prior != nil {
		return *prior, nil
	}
	if command.Kind == domain.DeliveryResumeKind {
		// Resume authenticates the exact stored barrier tuple under accepted
		// session authority. It must not read or substitute a current config.
		return repo.ApplyDeliveryCutover(ctx, authority, command, nil)
	}
	active, err := s.store.ReadActiveConfig(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		return repo.ApplyDeliveryCutover(ctx, authority, command, nil)
	}
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	resolved, err := s.decodeActive(ctx, active)
	if err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	// The desired binding is compared only as the cutover prerequisite. It never
	// supplies an applied-mode fact or changes any persisted effective mode.
	member := false
	for _, assignment := range resolved.Assignments {
		if assignment.Monitor != nil && assignment.Monitor.ID == command.MonitorID &&
			assignment.Generation == command.Generation && assignment.AssignmentRevision == command.AssignmentRevision &&
			assignment.DesiredAlertDelivery == command.AlertDelivery {
			member = true
			break
		}
	}
	if !member {
		return repo.ApplyDeliveryCutover(ctx, authority, command, nil)
	}
	return repo.ApplyDeliveryCutover(ctx, authority, command, &active)
}
