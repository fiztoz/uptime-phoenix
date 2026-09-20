package edge

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.DeliveryOutboxRepository = (*Store)(nil)

type edgeDeliveryRow struct {
	bun.BaseModel           `bun:"table:edge_delivery_outbox"`
	DeliveryID              string `bun:"delivery_id,pk"`
	SourceAlertID           string `bun:"source_alert_id"`
	SourceTransitionVersion int64  `bun:"source_transition_version"`
	NotificationID          int64  `bun:"notification_id"`
	NotificationVersion     int64  `bun:"notification_version"`
	EventKind               string `bun:"event_kind"`
	MonitorID               int64  `bun:"monitor_id,nullzero"`
	Generation              int64  `bun:"generation,nullzero"`
	SourceSeq               int64  `bun:"source_seq"`
	ConfigRevision          int64  `bun:"config_revision"`
	CheckStatus             int    `bun:"check_status"`
	CheckOutput             string `bun:"check_output"`
	ObservedAt              int64  `bun:"observed_at"`
	IncidentStatus          string `bun:"incident_status"`
	StartedAt               int64  `bun:"started_at"`
	ResolvedAt              *int64 `bun:"resolved_at"`
	AvailableAt             int64  `bun:"available_at"`
	Status                  string `bun:"status"`
	Attempt                 int64  `bun:"attempt"`
	LeaseToken              string `bun:"lease_token"`
	LeasedAt                *int64 `bun:"leased_at"`
	LeaseUntil              *int64 `bun:"lease_until"`
	ErrorCode               string `bun:"error_code"`
	OutcomeAt               *int64 `bun:"outcome_at"`
	CreatedAt               int64  `bun:"created_at"`
}

func (row *edgeDeliveryRow) queued(probeID, streamID string) domain.QueuedDelivery {
	return domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID:              row.DeliveryID,
			SourceAlertID:           row.SourceAlertID,
			SourceTransitionVersion: row.SourceTransitionVersion,
			ProbeID:                 probeID,
			NotificationID:          row.NotificationID,
			NotificationVersion:     row.NotificationVersion,
			EventKind:               row.EventKind,
			AvailableAt:             time.UnixMicro(row.AvailableAt).UTC(),
			EscalationPolicyID:      0,
			EscalationStep:          0,
		},
		MonitorID:            row.MonitorID,
		AssignmentGeneration: row.Generation,
		StreamID:             streamID,
		SourceSeq:            row.SourceSeq,
		ConfigRevision:       row.ConfigRevision,
		CheckStatus:          domain.Status(row.CheckStatus),
		CheckOutput:          row.CheckOutput,
		ObservedAt:           time.UnixMicro(row.ObservedAt).UTC(),
		IncidentStatus:       row.IncidentStatus,
		StartedAt:            time.UnixMicro(row.StartedAt).UTC(),
		ResolvedAt:           timeFromMicro(row.ResolvedAt),
		Status:               row.Status,
		Attempt:              row.Attempt,
		LeaseToken:           row.LeaseToken,
		LeasedAt:             timeFromMicro(row.LeasedAt),
		LeaseUntil:           timeFromMicro(row.LeaseUntil),
		ErrorCode:            row.ErrorCode,
		OutcomeAt:            timeFromMicro(row.OutcomeAt),
		CreatedAt:            time.UnixMicro(row.CreatedAt).UTC(),
	}
}

// ClaimDeliveries leases at most limit due intents in deterministic order.
// Expired leases are reclaimable with a new token and incremented attempt.
func (s *Store) ClaimDeliveries(ctx context.Context, probeID string, at time.Time, lease time.Duration, limit int) ([]domain.QueuedDelivery, error) {
	if at.IsZero() || lease < time.Second || lease > 15*time.Minute || limit < 1 || limit > 100 {
		return nil, domain.ErrValidation
	}
	if probeID == "" {
		return nil, domain.ErrValidation
	}

	atUTC := at.UTC().Truncate(time.Microsecond)
	untilUTC := atUTC.Add(lease)
	atMicro := atUTC.UnixMicro()
	untilMicro := untilUTC.UnixMicro()

	var claimed []domain.QueuedDelivery
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if probeID != i.ProbeID {
			return ports.ErrConflict
		}

		var rows []edgeDeliveryRow
		err := tx.NewSelect().Model(&rows).
			Where("(status IN (?, ?) AND available_at <= ?) OR (status = ? AND lease_until <= ?)",
				domain.DeliveryStatusPending, domain.DeliveryStatusRetrying, atMicro,
				domain.DeliveryStatusLeased, atMicro).
			Where("attempt < ?", int64(math.MaxInt64)).
			OrderExpr("available_at ASC, created_at ASC, delivery_id ASC").
			Limit(limit).
			Scan(ctx)
		if err != nil {
			return err
		}

		// Reserve bounded telemetry space before authorizing any provider I/O.
		// Expired leased work retains its reservation until an outcome commits.
		if len(rows) > 0 {
			var queuedBytes, leased int64
			if err := tx.NewRaw("SELECT COALESCE(SUM(length(payload)),0) FROM edge_telemetry_outbox").Scan(ctx, &queuedBytes); err != nil {
				return err
			}
			if err := tx.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE status = 'leased'").Scan(ctx, &leased); err != nil {
				return err
			}
			if s.retention.MaxBytes > 0 {
				// A configured small queue may not hold limit maximum outcomes.
				// Claim the safe prefix, keeping room for bounded loss metadata,
				// so one oversized request cannot starve every provider forever.
				capacity := (s.retention.MaxBytes - maxRetainedGaps*queueRowBytes) / (maxTelemetryEventBytes + queueRowBytes)
				selected := rows[:0]
				for _, row := range rows {
					if row.Status != domain.DeliveryStatusLeased {
						if leased >= capacity {
							continue
						}
						leased++
					}
					selected = append(selected, row)
				}
				rows = selected
				if err := s.retainTelemetry(ctx, tx, atUTC, "delivery.result", 0, leased*(maxTelemetryEventBytes+queueRowBytes)); err != nil {
					if errors.Is(err, ErrQueueFull) {
						// Commit bounded eviction progress, but authorize no I/O.
						// A later claim can finish freeing the required capacity.
						return nil
					}
					return err
				}
			} else {
				for _, row := range rows {
					if row.Status != domain.DeliveryStatusLeased {
						leased++
					}
				}
				if queuedBytes > maxTelemetryQueueBytes-leased*maxTelemetryEventBytes {
					return ErrQueueFull
				}
			}
		}

		for idx := range rows {
			row := &rows[idx]
			if row.Attempt == math.MaxInt64 {
				continue
			}
			u, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			token := u.String()
			newAttempt := row.Attempt + 1

			_, err = tx.NewUpdate().Model((*edgeDeliveryRow)(nil)).
				Set("status = ?", domain.DeliveryStatusLeased).
				Set("attempt = ?", newAttempt).
				Set("lease_token = ?", token).
				Set("leased_at = ?", atMicro).
				Set("lease_until = ?", untilMicro).
				Where("delivery_id = ?", row.DeliveryID).
				Exec(ctx)
			if err != nil {
				return err
			}

			row.Status = domain.DeliveryStatusLeased
			row.Attempt = newAttempt
			row.LeaseToken = token
			row.LeasedAt = &atMicro
			row.LeaseUntil = &untilMicro

			claimed = append(claimed, row.queued(i.ProbeID, i.StreamID))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return claimed, nil
}

// FinishDelivery atomically updates the queue and corresponding outcome.
// Only the current, unexpired attempt may complete. An identical retry of a
// completed receipt is a no-op; conflicting or superseded receipts fail.
func (s *Store) FinishDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error {
	if s.telemetry == nil {
		return domain.ErrValidation
	}
	if !domain.ValidHubID(claim.DeliveryID) || claim.ProbeID == "" || claim.Attempt < 1 || len(claim.LeaseToken) != 36 {
		return domain.ErrValidation
	}
	if err := validateEdgeDeliveryResult(result); err != nil {
		return err
	}

	resultAt := result.At.UTC().Truncate(time.Microsecond)
	resultAtMicro := resultAt.UnixMicro()
	var retryAtMicro *int64
	if result.Status == domain.DeliveryStatusRetrying {
		micro := result.RetryAt.UTC().Truncate(time.Microsecond).UnixMicro()
		retryAtMicro = &micro
	}

	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if claim.ProbeID != i.ProbeID {
			return ports.ErrConflict
		}

		var row edgeDeliveryRow
		err := tx.NewSelect().Model(&row).Where("delivery_id = ?", claim.DeliveryID).Scan(ctx)
		if err != nil {
			return err
		}

		// Stale attempt or lease token mismatch fails
		if row.Attempt != claim.Attempt || row.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}

		// Idempotent completed receipt check
		if row.Status != domain.DeliveryStatusLeased {
			if sameEdgeDeliveryResult(&row, result, resultAtMicro, retryAtMicro) {
				return nil
			}
			return ports.ErrConflict
		}

		// Only current unexpired claim may complete
		if row.LeaseUntil == nil || row.LeasedAt == nil {
			return ports.ErrConflict
		}
		if resultAtMicro < *row.LeasedAt || resultAtMicro >= *row.LeaseUntil {
			return ports.ErrConflict
		}

		// Sequence counter overflow check
		if i.LastCreatedSeq > math.MaxInt64-1 {
			return ports.ErrConflict
		}
		nextSeq := i.LastCreatedSeq + 1

		d := domain.RegionalDelivery{
			DeliveryID:              row.DeliveryID,
			SourceAlertID:           row.SourceAlertID,
			SourceTransitionVersion: row.SourceTransitionVersion,
			ProbeID:                 i.ProbeID,
			NotificationID:          row.NotificationID,
			NotificationVersion:     row.NotificationVersion,
			EventKind:               row.EventKind,
			Attempt:                 row.Attempt,
			Status:                  result.Status,
			ErrorCode:               result.ErrorCode,
			ObservedAt:              resultAt,
		}

		payload, err := s.telemetry.EncodeDelivery(nextSeq, d)
		if err != nil {
			return domain.ErrValidation
		}

		if err := s.appendTelemetry(ctx, tx, nextSeq, "delivery.result", resultAt, payload); err != nil {
			return err
		}

		upd := tx.NewUpdate().Model((*edgeDeliveryRow)(nil)).
			Set("status = ?", result.Status).
			Set("lease_until = NULL").
			Set("error_code = ?", result.ErrorCode).
			Set("outcome_at = ?", resultAtMicro)
		if retryAtMicro != nil {
			upd = upd.Set("available_at = ?", *retryAtMicro)
		}
		if _, err := upd.Where("delivery_id = ?", row.DeliveryID).Exec(ctx); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", nextSeq); err != nil {
			return err
		}

		return nil
	})
}

// GetDeliveryIntent reads only the requested probe's source work.
func (s *Store) GetDeliveryIntent(ctx context.Context, probeID, deliveryID string) (*domain.QueuedDelivery, error) {
	if probeID == "" || !domain.ValidHubID(deliveryID) {
		return nil, domain.ErrValidation
	}
	i, err := s.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if probeID != i.ProbeID {
		return nil, ports.ErrNotFound
	}
	var row edgeDeliveryRow
	err = s.db.NewSelect().Model(&row).Where("delivery_id = ?", deliveryID).Scan(ctx)
	if err != nil {
		return nil, storageError(ctx, err)
	}
	out := row.queued(i.ProbeID, i.StreamID)
	return &out, nil
}

func validateEdgeDeliveryResult(result domain.DeliveryResult) error {
	if result.At.IsZero() {
		return domain.ErrValidation
	}
	switch result.Status {
	case domain.DeliveryStatusRetrying, domain.DeliveryStatusFailed:
		if !validDeliveryErrorCode(result.ErrorCode) {
			return domain.ErrValidation
		}
	case domain.DeliveryStatusSent, domain.DeliveryStatusSuperseded:
		if result.ErrorCode != "" {
			return domain.ErrValidation
		}
	default:
		return domain.ErrValidation
	}
	if result.Status == domain.DeliveryStatusRetrying {
		if result.RetryAt.IsZero() || !result.RetryAt.UTC().Truncate(time.Microsecond).After(result.At.UTC().Truncate(time.Microsecond)) {
			return domain.ErrValidation
		}
	} else if !result.RetryAt.IsZero() {
		return domain.ErrValidation
	}
	return nil
}

func validDeliveryErrorCode(code string) bool {
	if len(code) < 1 || len(code) > 128 || code[0] < 'a' || code[0] > 'z' {
		return false
	}
	for _, c := range code {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

func sameEdgeDeliveryResult(row *edgeDeliveryRow, result domain.DeliveryResult, resultAtMicro int64, retryAtMicro *int64) bool {
	if row.Status != result.Status || row.ErrorCode != result.ErrorCode {
		return false
	}
	if row.OutcomeAt == nil || *row.OutcomeAt != resultAtMicro {
		return false
	}
	if result.Status == domain.DeliveryStatusRetrying {
		if retryAtMicro == nil || row.AvailableAt != *retryAtMicro {
			return false
		}
	}
	return true
}

// Source recorders validate scope/lifecycle before sharing this bounded insert.
// NULL monitor/generation represent a probe entity, never a fabricated monitor.
func insertEdgeQueuedDelivery(ctx context.Context, tx bun.Tx, item domain.QueuedDelivery) error {
	var retainedBytes int64
	if err := tx.NewRaw("SELECT COALESCE(SUM(length(CAST(check_output AS BLOB)) + 1024), 0) FROM edge_delivery_outbox").Scan(ctx, &retainedBytes); err != nil {
		return err
	}
	if retainedBytes > maxDeliveryQueueBytes-int64(len(item.CheckOutput))-1024 {
		return ErrQueueFull
	}
	row := edgeDeliveryRow{DeliveryID: item.DeliveryID, SourceAlertID: item.SourceAlertID, SourceTransitionVersion: item.SourceTransitionVersion, NotificationID: item.NotificationID, NotificationVersion: item.NotificationVersion, EventKind: item.EventKind, MonitorID: item.MonitorID, Generation: item.AssignmentGeneration, SourceSeq: item.SourceSeq, ConfigRevision: item.ConfigRevision, CheckStatus: int(item.CheckStatus), CheckOutput: item.CheckOutput, ObservedAt: item.ObservedAt.UTC().UnixMicro(), IncidentStatus: item.IncidentStatus, StartedAt: item.StartedAt.UTC().UnixMicro(), ResolvedAt: microFromTime(item.ResolvedAt), AvailableAt: item.AvailableAt.UTC().UnixMicro(), Status: domain.DeliveryStatusPending, CreatedAt: item.CreatedAt.UTC().UnixMicro()}
	_, err := tx.NewInsert().Model(&row).Exec(ctx)
	return err
}
