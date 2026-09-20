package edge

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeDeliveryRepository = (*Store)(nil)

// AuthorizeEdgeDelivery serializes the final lifecycle read against ACK and
// source recording. The returned item comes from storage, not the caller.
func (s *Store) AuthorizeEdgeDelivery(ctx context.Context, claim domain.DeliveryClaim, config domain.ProbeConfigMetadata, budget time.Duration) (*domain.QueuedDelivery, error) {
	if !domain.ValidHubID(claim.DeliveryID) || claim.Attempt < 1 || claim.LeaseToken == "" || !domain.ValidProbeConfigMetadata(config) || claim.ProbeID != config.ProbeID || budget <= 0 || budget > 30*time.Second {
		return nil, domain.ErrValidation
	}
	var out *domain.QueuedDelivery
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if identity.ProbeID != claim.ProbeID || identity.HubID != config.HubID {
			return ports.ErrConflict
		}
		active, err := readActiveConfig(ctx, tx)
		if err != nil {
			return err
		}
		if !domain.SameProbeConfigMetadata(active.Snapshot.ProbeConfigMetadata, config) {
			return nil
		}
		var row edgeDeliveryRow
		if err := tx.NewSelect().Model(&row).Where("delivery_id = ?", claim.DeliveryID).Scan(ctx); err != nil {
			return err
		}
		item := row.queued(identity.ProbeID, identity.StreamID)
		if item.Status != domain.DeliveryStatusLeased || item.Attempt != claim.Attempt || item.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}
		// Use database time for the authority check, including its millisecond
		// precision. Source observation time never extends a provider lease.
		var timestamp string
		if err := tx.NewRaw("SELECT strftime('%Y-%m-%d %H:%M:%f','now')").Scan(ctx, &timestamp); err != nil {
			return err
		}
		now, err := time.ParseInLocation("2006-01-02 15:04:05.999999", timestamp, time.UTC)
		if err != nil {
			return err
		}
		if item.LeasedAt == nil || now.Before(item.LeasedAt.UTC().Truncate(time.Millisecond)) || item.LeaseUntil == nil || !now.Add(budget).Before(*item.LeaseUntil) {
			return ports.ErrConflict
		}
		var rowIncident edgeIncidentRow
		if err := tx.NewSelect().Model(&rowIncident).Where("source_alert_id = ?", item.SourceAlertID).Scan(ctx); err != nil {
			return err
		}
		inc := rowIncident.incident(identity.ProbeID)
		if inc.Scope != domain.IncidentScopeRegional || inc.SubjectKind != domain.IncidentSubjectAvailability || inc.MonitorID != item.MonitorID || inc.AssignmentGeneration != item.AssignmentGeneration || inc.TransitionVersion != item.SourceTransitionVersion || !inc.StartedAt.Equal(item.StartedAt) {
			return nil
		}
		switch item.CheckStatus {
		case domain.StatusDown:
			if inc.Status != domain.AlertStatusFiring || inc.AckedAt != nil {
				return nil
			}
		case domain.StatusUp:
			if inc.Status != domain.AlertStatusResolved || inc.ResolvedAt == nil || item.ResolvedAt == nil || !inc.ResolvedAt.Equal(*item.ResolvedAt) {
				return nil
			}
		default:
			return nil
		}
		out = &item
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
