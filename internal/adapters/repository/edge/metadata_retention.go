package edge

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const maxMetadataBytes = 64 << 20

// Retire only old terminal metadata. Serialized telemetry and command receipts
// own independent retention rules and are never rewritten or ACKed here.
func (s *Store) retainMetadata(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity, now time.Time) error {
	cutoff := now.UTC().Add(-max(s.retention.MaxAge, 7*24*time.Hour)).UnixMicro()
	queries := []string{
		`DELETE FROM edge_delivery_outbox WHERE delivery_id IN (
 SELECT delivery_id FROM edge_delivery_outbox WHERE status IN ('sent','failed','superseded') AND outcome_at < ?
 ORDER BY outcome_at, delivery_id LIMIT 512)`,
		`DELETE FROM edge_regional_state WHERE (monitor_id,generation) IN (
 SELECT s.monitor_id,s.generation FROM edge_regional_state s WHERE s.observed_at < ?
 AND NOT EXISTS (SELECT 1 FROM edge_assignments a WHERE a.monitor_id=s.monitor_id AND a.generation=s.generation AND a.revision=? AND a.active=1)
 AND NOT EXISTS (SELECT 1 FROM edge_alerts a WHERE a.monitor_id=s.monitor_id AND a.generation=s.generation AND a.status IN ('firing','acked'))
 ORDER BY s.observed_at,s.monitor_id,s.generation LIMIT 512)`,
		`DELETE FROM edge_alerts WHERE source_alert_id IN (
 SELECT a.source_alert_id FROM edge_alerts a WHERE a.status='resolved' AND a.resolved_at < ?
 AND NOT EXISTS (SELECT 1 FROM edge_delivery_outbox d WHERE d.source_alert_id=a.source_alert_id)
 AND NOT EXISTS (SELECT 1 FROM edge_regional_state s WHERE s.source_alert_id=a.source_alert_id)
 AND NOT EXISTS (SELECT 1 FROM edge_watchdog_state w WHERE w.source_alert_id=a.source_alert_id)
 ORDER BY a.resolved_at,a.source_alert_id LIMIT 512)`,
		`DELETE FROM edge_config WHERE revision IN (
 SELECT c.revision FROM edge_config c WHERE c.applied_at < ? AND c.revision<>?
 AND NOT EXISTS (SELECT 1 FROM edge_regional_state s WHERE s.config_revision=c.revision)
 AND NOT EXISTS (SELECT 1 FROM edge_alerts a WHERE a.config_revision=c.revision)
 AND NOT EXISTS (SELECT 1 FROM edge_delivery_outbox d WHERE d.config_revision=c.revision)
 AND NOT EXISTS (SELECT 1 FROM edge_watchdog_state w WHERE w.config_revision=c.revision)
 ORDER BY c.applied_at,c.revision LIMIT 512)`,
	}
	for n, query := range queries {
		args := []any{cutoff}
		if n == 1 || n == 3 {
			args = append(args, i.ConfigRevision)
		}
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}
