package edge

import (
	"context"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ListDueEscalations returns pending firing ladders whose next step is due.
func (s *Store) ListDueEscalations(ctx context.Context, now time.Time, limit int) ([]domain.RegionalIncident, error) {
	if limit <= 0 || limit > 100 || now.IsZero() {
		return nil, domain.ErrValidation
	}
	var rows []edgeIncidentRow
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return tx.NewSelect().Model(&rows).
			Where("status = ?", domain.AlertStatusFiring).
			Where("escalation_status = ?", domain.EscalationStatePending).
			Where("escalation_next_run_at <= ?", now.UTC().UnixMicro()).
			OrderExpr("escalation_next_run_at ASC, source_alert_id ASC").
			Limit(limit).
			Scan(ctx)
	})
	if err != nil {
		return nil, storageError(ctx, err)
	}
	identity, err := s.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.RegionalIncident, 0, len(rows))
	for _, row := range rows {
		out = append(out, *row.incident(identity.ProbeID))
	}
	return out, nil
}

// CommitEscalationAdvance writes one ladder transition and the step's intents.
// Provider I/O stays outside this transaction. A version mismatch is stale.
func (s *Store) CommitEscalationAdvance(ctx context.Context, expectedVersion int64, incident domain.RegionalIncident, intents []domain.DeliveryIntent, message string, at time.Time) error {
	if s.telemetry == nil || expectedVersion <= 0 || at.IsZero() || incident.TransitionVersion != expectedVersion+1 || len(message) > 4096 || len(intents) > 1000 || !domain.ValidIncidentEscalation(&incident) || incident.Status != domain.AlertStatusFiring {
		return domain.ErrValidation
	}
	at = at.UTC()
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		identity, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		var stored edgeIncidentRow
		if err := tx.NewSelect().Model(&stored).Where("source_alert_id = ?", incident.SourceAlertID).Scan(ctx); err != nil {
			return storageError(ctx, err)
		}
		prior := stored.incident(identity.ProbeID)
		if prior.Status != domain.AlertStatusFiring || prior.TransitionVersion != expectedVersion || prior.EscalationStatus != domain.EscalationStatePending || prior.EscalationPolicyID != incident.EscalationPolicyID || prior.MonitorID != incident.MonitorID || prior.AssignmentGeneration != incident.AssignmentGeneration || prior.ProbeID != incident.ProbeID || identity.ProbeID != incident.ProbeID {
			return ports.ErrStaleLocalState
		}
		if identity.LastCreatedSeq == math.MaxInt64 {
			return ports.ErrConflict
		}
		seq := identity.LastCreatedSeq + 1
		row := newEdgeIncidentRow(incident)
		result, err := tx.NewUpdate().Model(&row).WherePK().Where("transition_version = ?", expectedVersion).Exec(ctx)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			return ports.ErrStaleLocalState
		}
		payload, err := s.telemetry.EncodeIncident(seq, at, incident)
		if err != nil {
			return domain.ErrValidation
		}
		if err := s.appendTelemetry(ctx, tx, seq, "alert.transition", at, payload); err != nil {
			return err
		}
		for _, intent := range intents {
			if intent.EscalationPolicyID != incident.EscalationPolicyID || intent.EscalationStep <= 0 || intent.SourceTransitionVersion != incident.TransitionVersion || intent.EventKind != domain.DeliveryEventStatusChange || intent.ProbeID != identity.ProbeID || intent.SourceAlertID != incident.SourceAlertID || intent.NotificationVersion != incident.ConfigRevision {
				return domain.ErrValidation
			}
			queued := domain.QueuedDelivery{DeliveryIntent: intent, MonitorID: incident.MonitorID, AssignmentGeneration: incident.AssignmentGeneration, StreamID: identity.StreamID, SourceSeq: seq, ConfigRevision: incident.ConfigRevision, CheckStatus: domain.StatusDown, CheckOutput: message, ObservedAt: at, IncidentStatus: incident.Status, StartedAt: incident.StartedAt, CreatedAt: at}
			if err := insertEdgeQueuedDelivery(ctx, tx, queued); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", seq)
		return err
	})
}
