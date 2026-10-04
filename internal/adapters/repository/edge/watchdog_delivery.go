package edge

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeWatchdogDeliveryRepository = (*Store)(nil)

// AuthorizeWatchdogDelivery rechecks local exclusive ownership, accepted config,
// source lifecycle and the current attempt immediately before bounded provider I/O.
func (s *Store) AuthorizeWatchdogDelivery(ctx context.Context, a domain.ProbeWatchdogAuthority, claim domain.DeliveryClaim, config domain.ProbeConfigMetadata, budget time.Duration) (*domain.QueuedDelivery, error) {
	if !validEdgeWatchdogAuthority(a) || a.HealthGeneration != 0 || claim.ProbeID != a.ProbeID || !domain.ValidHubID(claim.DeliveryID) || claim.Attempt < 1 || claim.LeaseToken == "" || !domain.ValidProbeConfigMetadata(config) || config.HubID != a.HubID || config.ProbeID != a.ProbeID || budget <= 0 || budget > 30*time.Second {
		return nil, domain.ErrValidation
	}
	var out *domain.QueuedDelivery
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if err := checkEdgeWatchdogAuthority(a, i); err != nil {
			return err
		}
		active, err := readActiveConfig(ctx, tx)
		if err != nil {
			return err
		}
		if !domain.SameProbeConfigMetadata(active.Snapshot.ProbeConfigMetadata, config) {
			return nil
		}
		var row edgeDeliveryRow
		if err := tx.NewSelect().Model(&row).Where("delivery_id=?", claim.DeliveryID).Scan(ctx); err != nil {
			return err
		}
		item := row.queued(i.ProbeID, i.StreamID)
		if item.Status != domain.DeliveryStatusLeased || item.Attempt != claim.Attempt || item.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}
		var incident edgeIncidentRow
		if err := tx.NewSelect().Model(&incident).Where("source_alert_id=?", item.SourceAlertID).Scan(ctx); err != nil {
			return err
		}
		if item.ConfigRevision != config.Revision || item.NotificationVersion != config.Revision || !domain.ProbeWatchdogDeliveryEligible(item, incident.incident(i.ProbeID)) {
			return nil
		}
		// The DB owns expiry; source wall time and peer-reported clock are not authority.
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
		out = &item
		return nil
	})
	if err != nil {
		return nil, storageError(ctx, err)
	}
	return out, nil
}
