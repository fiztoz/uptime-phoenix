package services

import (
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Delivery configuration evidence states are observational, never mode authority.
const (
	DeliveryConfigAcknowledged       = "acknowledged"
	DeliveryConfigUnreported         = "unreported"
	DeliveryConfigUnverified         = "unverified"
	DeliveryConfigAssignmentMissing  = "assignment_missing"
	DeliveryConfigGenerationMismatch = "generation_mismatch"
	DeliveryConfigLocalUnavailable   = "local_unavailable"
)

// ErrAssignmentReadChanged refuses a mixed desired-set/evidence read. It is a
// retryable read conflict, not a CAS write or a lease-authorization fence.
var ErrAssignmentReadChanged = errors.New("assignment revision changed during read")

// DeliveryConfigEvidence contains nonsecret facts for one current assignment.
// Counters and mode stay zero unless a verified document contains that exact
// monitor and generation. AlertDelivery is desired document content only.
type DeliveryConfigEvidence struct {
	State          string
	ConfigRevision int64
	Generation     int64
	AlertDelivery  domain.AlertDelivery
}

// SetDeliveryConfigReader binds the trusted hub identity and nonsecret reader
// before serving requests. Local evidence has a separate optional reader.
func (s *MonitorRegionalService) SetDeliveryConfigReader(hubID string, reader ports.RemoteProbeDeliveryConfigReader) error {
	if s == nil || reader == nil || !domain.ValidHubID(hubID) {
		return domain.ErrValidation
	}
	s.deliveryHub, s.delivery = hubID, reader
	return nil
}

// SetLocalDeliveryConfigReader adds local document evidence before serving.
// Missing local wiring retains the explicit local_unavailable diagnostic state.
func (s *MonitorRegionalService) SetLocalDeliveryConfigReader(reader ports.LocalProbeDeliveryConfigReader) error {
	if s == nil || reader == nil {
		return domain.ErrValidation
	}
	s.localDelivery = reader
	return nil
}

// AcknowledgedDeliveryConfigs reads opt-in document evidence only after scoped
// access checks. A revision/grant change refuses the assembled response. None
// of these point-in-time reads authorizes applying a mode, writing or sending.
func (s *MonitorRegionalService) AcknowledgedDeliveryConfigs(ctx context.Context, userID, monitorID, expectedRevision int64, now time.Time) (map[string]DeliveryConfigEvidence, error) {
	if expectedRevision < 0 {
		return nil, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	current, err := s.Assignments(ctx, userID, monitorID, now)
	if err != nil {
		return nil, err
	}
	if current.Set.Revision != expectedRevision {
		return nil, ErrAssignmentReadChanged
	}
	if s.delivery == nil || !domain.ValidHubID(s.deliveryHub) {
		return nil, domain.ErrInternal
	}
	result := make(map[string]DeliveryConfigEvidence, len(current.Set.Assignments))
	for _, member := range current.Set.Assignments {
		if member.ProbeID == domain.LocalProbeID && s.localDelivery == nil {
			result[member.ProbeID] = DeliveryConfigEvidence{State: DeliveryConfigLocalUnavailable}
			continue
		}
		target := domain.ProbeConfigTarget{HubID: s.deliveryHub, ProbeID: member.ProbeID}
		if !domain.ValidProbeConfigTarget(target) {
			return nil, domain.ErrInternal
		}
		var document *domain.AcknowledgedProbeDeliveryConfig
		var readErr error
		if member.ProbeID == domain.LocalProbeID {
			document, readErr = s.localDelivery.ReadAcknowledgedLocalDelivery(ctx, target)
		} else {
			document, readErr = s.delivery.ReadAcknowledgedRemoteDelivery(ctx, target)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch {
		case errors.Is(readErr, context.Canceled):
			return nil, context.Canceled
		case errors.Is(readErr, context.DeadlineExceeded):
			return nil, context.DeadlineExceeded
		case errors.Is(readErr, ports.ErrNotFound):
			result[member.ProbeID] = DeliveryConfigEvidence{State: DeliveryConfigUnreported}
			continue
		case errors.Is(readErr, ports.ErrConflict):
			result[member.ProbeID] = DeliveryConfigEvidence{State: DeliveryConfigUnverified}
			continue
		case readErr != nil:
			return nil, domain.ErrInternal // Never propagate confidential decoder details.
		}
		if document == nil || document.Target != target || document.Revision <= 0 {
			return nil, domain.ErrInternal
		}
		var selected *domain.AcknowledgedAssignmentDelivery
		for index := range document.Assignments {
			entry := &document.Assignments[index]
			if entry.MonitorID == monitorID {
				if selected != nil {
					return nil, domain.ErrInternal
				}
				selected = entry
			}
		}
		evidence := DeliveryConfigEvidence{State: DeliveryConfigAssignmentMissing}
		if selected != nil {
			evidence.State = DeliveryConfigGenerationMismatch
			if selected.Generation == member.Generation {
				mode, valid := domain.CanonicalAlertDelivery(selected.AlertDelivery)
				if !valid || selected.Generation <= 0 {
					return nil, domain.ErrInternal
				}
				evidence = DeliveryConfigEvidence{State: DeliveryConfigAcknowledged, ConfigRevision: document.Revision, Generation: selected.Generation, AlertDelivery: mode}
			}
		}
		result[member.ProbeID] = evidence
	}
	// An access revocation or new assignment revision must not escape as a
	// response for the earlier set. This is still not a transactional write fence.
	if err := s.health.denyIfHidden(ctx, userID, monitorID); err != nil {
		return nil, err
	}
	_, latest, _, err := s.health.loadMonitorEvidence(ctx, monitorID)
	if err != nil {
		return nil, err
	}
	if latest.Revision != expectedRevision {
		return nil, ErrAssignmentReadChanged
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
