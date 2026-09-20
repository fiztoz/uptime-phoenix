package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.ProbeWatchdogDeliveryRepository = (*ProbeWatchdogStore)(nil)

// AuthorizeWatchdogDelivery fences a hub source send under the current runtime,
// applied graph, incident and delivery attempt. No lock spans provider I/O.
func (s *ProbeWatchdogStore) AuthorizeWatchdogDelivery(ctx context.Context, a domain.ProbeWatchdogAuthority, claim domain.DeliveryClaim, config domain.ProbeConfigMetadata, budget time.Duration) (*domain.QueuedDelivery, error) {
	if s == nil || s.db == nil || s.protector == nil || !validHubWatchdogAuthority(a) || a.HealthGeneration != 0 || claim.ProbeID != a.ProbeID || !domain.ValidHubID(claim.DeliveryID) || claim.Attempt < 1 || claim.LeaseToken == "" || !domain.ValidProbeConfigMetadata(config) || config.HubID != a.HubID || config.ProbeID != a.ProbeID || budget <= 0 || budget > 30*time.Second {
		return nil, domain.ErrValidation
	}
	// Follow the existing parent -> intent lock order. The immutable parent ID
	// is discovered before the transaction and rechecked under its locks.
	var sourceID string
	if err := s.db.NewSelect().Table("probe_delivery_intents").Column("source_alert_id").Where("probe_id=? AND delivery_id=?", a.ProbeID, claim.DeliveryID).Scan(ctx, &sourceID); err != nil {
		return nil, watchdogStorageError(ctx, err)
	}
	var out *domain.QueuedDelivery
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		parent, err := s.lockAuthority(ctx, tx, a)
		if err != nil {
			return err
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id=?", a.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if active.HubID != config.HubID || active.Revision != config.Revision || active.SHA256 != config.SHA256 {
			return nil
		}
		incident, err := lockDeliveryIncident(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		owned, err := hubOwnsWatchdogIncident(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if !owned {
			return ports.ErrConflict
		}
		var row deliveryIntentModel
		if err := tx.NewSelect().Model(&row).Where("probe_id=? AND delivery_id=?", a.ProbeID, claim.DeliveryID).Scan(ctx); err != nil {
			return err
		}
		item := row.queued()
		if item.SourceAlertID != sourceID || item.StreamID != "" || item.Status != domain.DeliveryStatusLeased || item.Attempt != claim.Attempt || item.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}
		inc := incidentFromModel(incident)
		if item.ConfigRevision != config.Revision || item.NotificationVersion != config.Revision || !domain.ProbeWatchdogDeliveryEligible(item, &inc) {
			return nil
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if item.LeasedAt == nil || item.LeaseUntil == nil {
			return ports.ErrConflict
		}
		started := item.LeasedAt.UTC()
		if tx.Dialect().Name() == dialect.SQLite {
			started = started.Truncate(time.Millisecond)
		}
		if now.Before(started) || !now.Add(budget).Before(*item.LeaseUntil) || !now.Add(budget).Before(time.Unix(parent.LeaseUntil, 0).UTC()) {
			return ports.ErrConflict
		}
		out = &item
		return nil
	})
	if err != nil {
		return nil, watchdogStorageError(ctx, err)
	}
	return out, nil
}
