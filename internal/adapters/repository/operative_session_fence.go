package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// requireOperativeSessionsTx checks current stored remote source authority after
// expensive capture and lock waits. A handshake from a former generation does
// not prove this generation has the compiled runtime. Locks remain held through
// the grant commit; the database clock, not a caller clock, bounds its budget.
func (r *OperativeDeliveryStore) requireOperativeSessionsTx(ctx context.Context, tx bun.Tx, hub string, members []operativeMemberModel, budget time.Duration) error {
	for _, member := range members {
		if member.ProbeID == domain.LocalProbeID {
			continue
		}
		lease, err := readProbeSession(ctx, tx, member.ProbeID)
		if err != nil {
			return err
		}
		session := domain.ProbeReplaySession{HubID: hub, ProbeID: member.ProbeID, StreamID: member.StreamID, ConnectionGeneration: lease.Generation, OwnerID: lease.OwnerID}
		authority, err := lockProbeSession(ctx, tx, session, r.protector.KeyHash(hub))
		if err != nil {
			return err
		}
		if authority.lease.LeaseUntil <= authority.now.Add(budget).Unix() {
			return ports.ErrConflict
		}
		var capability operativeCapabilityModel
		if err := tx.NewSelect().Model(&capability).Where("probe_id = ? AND capability = ?", member.ProbeID, domain.DeliveryCutoverCapability).Scan(ctx); err != nil {
			return ports.ErrConflict
		}
		if capability.ConnectionGeneration != authority.lease.Generation {
			return ports.ErrConflict
		}
	}
	return nil
}
