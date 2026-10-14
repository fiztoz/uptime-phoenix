package probe

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalDeliveryConfigDecoder validates acknowledged local documents and exposes
// only nonsecret identity/mode facts. It never creates execution inputs or work.
type LocalDeliveryConfigDecoder struct {
	validator *LocalConfigValidator
}

var _ ports.LocalProbeDeliveryConfigDecoder = (*LocalDeliveryConfigDecoder)(nil)

// NewLocalDeliveryConfigDecoder binds installed extension validators without I/O.
func NewLocalDeliveryConfigDecoder(checker func(string) (ports.Checker, bool), sender func(string) (ports.NotificationSender, bool)) *LocalDeliveryConfigDecoder {
	return &LocalDeliveryConfigDecoder{validator: NewLocalConfigValidator(checker, sender)}
}

// DecodeLocalDelivery authenticates document metadata identity and validates the
// existing local dialect before projecting desired document modes, not runtime
// application or send/suppression authority. Errors contain no private settings.
func (d *LocalDeliveryConfigDecoder) DecodeLocalDelivery(ctx context.Context, document []byte, expected domain.ProbeConfigMetadata) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d == nil || d.validator == nil || d.validator.checker == nil || d.validator.sender == nil || !domain.ValidProbeConfigMetadata(expected) || expected.ProbeID != domain.LocalProbeID {
		return nil, domain.ErrValidation
	}
	metadata, err := (ConfigInspector{}).Inspect(document, expected.ProbeConfigTarget)
	if err != nil {
		return nil, domain.ErrValidation
	}
	if !domain.SameProbeConfigMetadata(metadata, expected) {
		return nil, ports.ErrConflict
	}
	snapshot, err := DecodeLocalConfigSnapshot(document)
	if err != nil || snapshot.Watchdog.Enabled {
		return nil, domain.ErrValidation
	}
	if err := d.validator.validateSnapshot(ctx, snapshot); err != nil {
		return nil, err
	}
	result := &domain.AcknowledgedProbeDeliveryConfig{Target: expected.ProbeConfigTarget, Revision: expected.Revision,
		Assignments: make([]domain.AcknowledgedAssignmentDelivery, 0, len(snapshot.Assignments))}
	for _, a := range snapshot.Assignments {
		result.Assignments = append(result.Assignments, domain.AcknowledgedAssignmentDelivery{MonitorID: a.MonitorID, Generation: int64(a.Generation), AssignmentRevision: int64(a.AssignmentRevision), AlertDelivery: a.AlertDelivery})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
