package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type deliveryIntentModel struct {
	bun.BaseModel           `bun:"table:probe_delivery_intents,alias:intent"`
	DeliveryID              string     `bun:"delivery_id,pk"`
	SourceAlertID           string     `bun:"source_alert_id"`
	SourceTransitionVersion int64      `bun:"source_transition_version"`
	ProbeID                 string     `bun:"probe_id"`
	NotificationID          int64      `bun:"notification_id"`
	NotificationVersion     int64      `bun:"notification_version"`
	EventKind               string     `bun:"event_kind"`
	MonitorID               int64      `bun:"monitor_id"`
	AssignmentGeneration    int64      `bun:"assignment_generation"`
	StreamID                string     `bun:"stream_id"`
	SourceSeq               int64      `bun:"source_seq"`
	ConfigRevision          int64      `bun:"config_revision"`
	CheckStatus             int        `bun:"check_status"`
	CheckOutput             string     `bun:"check_output"`
	ObservedAt              time.Time  `bun:"observed_at"`
	IncidentStatus          string     `bun:"incident_status"`
	StartedAt               time.Time  `bun:"started_at"`
	ResolvedAt              *time.Time `bun:"resolved_at"`
	AvailableAt             time.Time  `bun:"available_at"`
	Status                  string     `bun:"status"`
	Attempt                 int64      `bun:"attempt"`
	LeaseToken              *string    `bun:"lease_token"`
	LeasedAt                *time.Time `bun:"leased_at"`
	LeaseUntil              *time.Time `bun:"lease_until"`
	ErrorCode               *string    `bun:"error_code"`
	OutcomeAt               *time.Time `bun:"outcome_at"`
	CreatedAt               time.Time  `bun:"created_at"`
}

var _ ports.DeliveryOutboxRepository = (*RegionalCommitStore)(nil)

func copyCommitIncident(incident *domain.RegionalIncident) *domain.RegionalIncident {
	if incident == nil {
		return nil
	}
	copy := *incident
	return &copy
}

func commitIncidentAndDeliveriesTx(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation, incident *domain.RegionalIncident, intents []domain.DeliveryIntent) error {
	if incident != nil {
		if err := bindCommitIncident(obs, incident); err != nil {
			return err
		}
		if err := putIncidentTx(ctx, tx, incident); err != nil {
			return err
		}
	}
	return enqueueDeliveryIntentsTx(ctx, tx, obs, incident, intents)
}

// GetDeliveryIntent returns durable source work in exactly one probe scope.
func (r *RegionalCommitStore) GetDeliveryIntent(ctx context.Context, probeID, deliveryID string) (*domain.QueuedDelivery, error) {
	if err := validateOutboxProbe(probeID); err != nil {
		return nil, err
	}
	id, err := canonicalIdentity("delivery_id", deliveryID)
	if err != nil {
		return nil, err
	}
	row := new(deliveryIntentModel)
	if err := r.db.NewSelect().Model(row).Where("delivery_id = ? AND probe_id = ?", id, probeID).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get delivery intent: %w", probeRegistryError(err))
	}
	out := row.queued()
	return &out, nil
}

// ClaimDeliveries reserves bounded due work without holding locks during I/O.
// MariaDB locks selected rows; SQLite obtains the writer lock before reading.
func (r *RegionalCommitStore) ClaimDeliveries(ctx context.Context, probeID string, at time.Time, lease time.Duration, limit int) ([]domain.QueuedDelivery, error) {
	if err := validateOutboxProbe(probeID); err != nil {
		return nil, err
	}
	if at.IsZero() || lease < time.Microsecond || lease > 15*time.Minute || limit < 1 || limit > 100 {
		return nil, fmt.Errorf("delivery claim bounds: %w", domain.ErrValidation)
	}
	at = outboxTime(at)
	until := outboxTime(at.Add(lease))
	var claimed []domain.QueuedDelivery
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := lockSQLiteOutbox(ctx, tx); err != nil {
			return err
		}
		var rows []deliveryIntentModel
		q := tx.NewSelect().Model(&rows).
			Where("intent.probe_id = ? AND intent.available_at <= ?", probeID, at).
			Where("intent.attempt < ?", int64(math.MaxInt64)).
			Where("(intent.status IN (?, ?) OR (intent.status = ? AND intent.lease_until <= ?))",
				domain.DeliveryStatusPending, domain.DeliveryStatusRetrying, domain.DeliveryStatusLeased, at).
			Where(`intent.assignment_generation = (SELECT assignment.generation FROM monitor_probe_assignments AS assignment
 WHERE assignment.monitor_id = intent.monitor_id AND assignment.probe_id = intent.probe_id
 AND assignment.active = ?)`, true).
			OrderExpr("intent.available_at ASC, intent.created_at ASC, intent.delivery_id ASC").Limit(limit)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE SKIP LOCKED")
		}
		if err := q.Scan(ctx); err != nil {
			return err
		}
		for i := range rows {
			row := &rows[i]
			token := rand.Text()
			row.Status, row.Attempt, row.LeaseToken, row.LeasedAt, row.LeaseUntil = domain.DeliveryStatusLeased, row.Attempt+1, &token, &at, &until
			if _, err := tx.NewUpdate().Model(row).Column("status", "attempt", "lease_token", "leased_at", "lease_until").WherePK().Exec(ctx); err != nil {
				return err
			}
			claimed = append(claimed, row.queued())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("claim deliveries: %w", err)
	}
	return claimed, nil
}

// FinishDelivery records one fenced result and its mirrored outcome atomically.
// Stored metadata, not caller-supplied channel or incident fields, owns the outcome.
func (r *RegionalCommitStore) FinishDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error {
	if err := validateOutboxProbe(claim.ProbeID); err != nil {
		return err
	}
	id, err := canonicalIdentity("delivery_id", claim.DeliveryID)
	if err != nil {
		return err
	}
	if claim.Attempt < 1 || claim.LeaseToken == "" || len(claim.LeaseToken) > 64 {
		return fmt.Errorf("delivery claim identity: %w", domain.ErrValidation)
	}
	if err := validateDeliveryResult(result); err != nil {
		return err
	}
	result.At = outboxTime(result.At)
	if !result.RetryAt.IsZero() {
		result.RetryAt = outboxTime(result.RetryAt)
		if !result.RetryAt.After(result.At) {
			return fmt.Errorf("delivery retry precision: %w", domain.ErrValidation)
		}
	}
	// Establish the immutable parent before taking locks, then use the same
	// parent -> intent order as enqueue/mirroring on MariaDB. Recheck the scoped
	// intent under its lock; deletion between these reads fails without effects.
	var sourceAlertID string
	if err := r.db.NewSelect().TableExpr("probe_delivery_intents").Column("source_alert_id").
		Where("delivery_id = ? AND probe_id = ?", id, claim.ProbeID).Scan(ctx, &sourceAlertID); err != nil {
		return probeRegistryError(err)
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := lockDeliveryIncident(ctx, tx, sourceAlertID); err != nil {
			return err
		}
		row := new(deliveryIntentModel)
		q := tx.NewSelect().Model(row).Where("delivery_id = ? AND probe_id = ?", id, claim.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); err != nil {
			return fmt.Errorf("finish delivery lookup: %w", probeRegistryError(err))
		}
		if row.Attempt != claim.Attempt || row.LeaseToken == nil || *row.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}
		if row.Status != domain.DeliveryStatusLeased {
			if sameDeliveryResult(row, result) {
				return nil
			}
			return ports.ErrConflict
		}
		if row.LeaseUntil == nil || row.LeasedAt == nil || result.At.Before(row.LeasedAt.UTC()) || !result.At.Before(row.LeaseUntil.UTC()) {
			return ports.ErrConflict
		}
		outcome := domain.RegionalDelivery{
			DeliveryID: row.DeliveryID, SourceAlertID: row.SourceAlertID, SourceTransitionVersion: row.SourceTransitionVersion,
			ProbeID: row.ProbeID, NotificationID: row.NotificationID, NotificationVersion: row.NotificationVersion,
			EventKind: row.EventKind, Attempt: row.Attempt, Status: result.Status, ErrorCode: result.ErrorCode, ObservedAt: result.At,
		}
		if err := putDeliveryTx(ctx, tx, &outcome, true); err != nil {
			return err
		}
		row.Status, row.LeaseUntil, row.OutcomeAt = result.Status, nil, &result.At
		row.ErrorCode = nil
		if result.ErrorCode != "" {
			row.ErrorCode = &result.ErrorCode
		}
		if result.Status == domain.DeliveryStatusRetrying {
			row.AvailableAt = result.RetryAt
		}
		_, err := tx.NewUpdate().Model(row).Column("status", "lease_until", "outcome_at", "error_code", "available_at").WherePK().Exec(ctx)
		return err
	})
}

func enqueueDeliveryIntentsTx(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation, incident *domain.RegionalIncident, intents []domain.DeliveryIntent) error {
	if len(intents) == 0 {
		return nil
	}
	if len(intents) > 100 || incident == nil || incident.SubjectKind != domain.IncidentSubjectAvailability ||
		incident.ConfigRevision != obs.ConfigRevision ||
		(incident.Status != domain.AlertStatusFiring && incident.Status != domain.AlertStatusResolved) ||
		(incident.Status == domain.AlertStatusFiring && obs.Status != domain.StatusDown) ||
		(incident.Status == domain.AlertStatusResolved && obs.Status != domain.StatusUp) || len(obs.Message) > 65535 {
		return fmt.Errorf("availability delivery context: %w", domain.ErrValidation)
	}
	for _, intent := range intents {
		id, err := canonicalIdentity("delivery_id", intent.DeliveryID)
		if err != nil {
			return err
		}
		sourceID, err := canonicalIdentity("source_alert_id", intent.SourceAlertID)
		if err != nil {
			return err
		}
		if sourceID != incident.SourceAlertID || intent.SourceTransitionVersion != incident.TransitionVersion || intent.ProbeID != incident.ProbeID {
			return fmt.Errorf("delivery incident identity: %w", ports.ErrConflict)
		}
		if intent.NotificationID < 1 || intent.NotificationVersion < 1 || intent.NotificationVersion != obs.ConfigRevision || intent.AvailableAt.IsZero() ||
			(intent.EventKind != domain.DeliveryEventStatusChange && intent.EventKind != domain.DeliveryEventIncidentSummary) ||
			(intent.EventKind == domain.DeliveryEventIncidentSummary && incident.Status != domain.AlertStatusResolved) {
			return fmt.Errorf("delivery intent: %w", domain.ErrValidation)
		}
		exists, err := deliveryIDExistsTx(ctx, tx, "probe_delivery_events", id)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("delivery identity already mirrored: %w", ports.ErrConflict)
		}
		row := deliveryIntentModel{
			DeliveryID: id, SourceAlertID: sourceID, SourceTransitionVersion: intent.SourceTransitionVersion,
			ProbeID: intent.ProbeID, NotificationID: intent.NotificationID, NotificationVersion: intent.NotificationVersion,
			EventKind: intent.EventKind, AvailableAt: outboxTime(intent.AvailableAt),
			MonitorID: obs.MonitorID, AssignmentGeneration: obs.AssignmentGeneration,
			StreamID: obs.StreamID, SourceSeq: obs.Seq, ConfigRevision: obs.ConfigRevision,
			CheckStatus: int(obs.Status), CheckOutput: obs.Message, ObservedAt: outboxTime(obs.ObservedAt),
			IncidentStatus: incident.Status, StartedAt: outboxTime(incident.StartedAt),
			Status: domain.DeliveryStatusPending, CreatedAt: outboxTime(obs.ReceivedAt),
		}
		if incident.ResolvedAt != nil {
			resolved := outboxTime(*incident.ResolvedAt)
			row.ResolvedAt = &resolved
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return fmt.Errorf("enqueue delivery: %w", probeRegistryError(err))
		}
	}
	return nil
}

func lockSQLiteOutbox(ctx context.Context, tx bun.Tx) error {
	if tx.Dialect().Name() != dialect.SQLite {
		return nil
	}
	// A write statement takes SQLite's database writer lock even with no rows.
	// Never read first and then try to upgrade a stale transaction snapshot.
	_, err := tx.ExecContext(ctx, "UPDATE probe_delivery_intents SET attempt = attempt WHERE 1 = 0")
	return err
}

func deliveryIDExistsTx(ctx context.Context, tx bun.Tx, table, id string) (bool, error) {
	var found string
	q := tx.NewSelect().Table(table).Column("delivery_id").Where("delivery_id = ?", id)
	if tx.Dialect().Name() == dialect.MySQL {
		// Use a current read after waiting on the incident lock; the transaction
		// may have read observation state before another writer committed.
		q = q.For("UPDATE")
	}
	err := q.Scan(ctx, &found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func lockSQLiteIncidents(ctx context.Context, tx bun.Tx) error {
	if tx.Dialect().Name() != dialect.SQLite {
		return nil
	}
	_, err := tx.ExecContext(ctx, "UPDATE probe_incidents SET transition_version = transition_version WHERE 1 = 0")
	return err
}

func lockDeliveryIncident(ctx context.Context, tx bun.Tx, id string) (*probeIncidentModel, error) {
	if err := lockSQLiteIncidents(ctx, tx); err != nil {
		return nil, err
	}
	row := new(probeIncidentModel)
	q := tx.NewSelect().Model(row).Where("source_alert_id = ?", id)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, probeRegistryError(err)
	}
	return row, nil
}

func validateOutboxProbe(probeID string) error {
	if probeID != domain.LocalProbeID && !validRemoteProbeID(probeID) {
		return fmt.Errorf("delivery probe: %w", domain.ErrValidation)
	}
	return nil
}

func validateDeliveryResult(result domain.DeliveryResult) error {
	if result.At.IsZero() {
		return fmt.Errorf("delivery result time: %w", domain.ErrValidation)
	}
	switch result.Status {
	case domain.DeliveryStatusRetrying, domain.DeliveryStatusFailed:
		if !validDeliveryErrorCode(result.ErrorCode) {
			return fmt.Errorf("delivery diagnostic code: %w", domain.ErrValidation)
		}
	case domain.DeliveryStatusSent, domain.DeliveryStatusSuperseded:
		if result.ErrorCode != "" {
			return fmt.Errorf("delivery diagnostic code: %w", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("delivery result status: %w", domain.ErrValidation)
	}
	if result.Status == domain.DeliveryStatusRetrying {
		if !result.RetryAt.After(result.At) {
			return fmt.Errorf("delivery retry time: %w", domain.ErrValidation)
		}
	} else if !result.RetryAt.IsZero() {
		return fmt.Errorf("terminal delivery retry: %w", domain.ErrValidation)
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

func sameDeliveryResult(row *deliveryIntentModel, result domain.DeliveryResult) bool {
	code := ""
	if row.ErrorCode != nil {
		code = *row.ErrorCode
	}
	return row.Status == result.Status && code == result.ErrorCode && row.OutcomeAt != nil && row.OutcomeAt.Equal(result.At) &&
		(result.Status != domain.DeliveryStatusRetrying || row.AvailableAt.Equal(result.RetryAt))
}

func outboxTime(at time.Time) time.Time { return at.UTC().Truncate(time.Microsecond) }

func (m *deliveryIntentModel) queued() domain.QueuedDelivery {
	out := domain.QueuedDelivery{
		DeliveryIntent: domain.DeliveryIntent{
			DeliveryID: m.DeliveryID, SourceAlertID: m.SourceAlertID, SourceTransitionVersion: m.SourceTransitionVersion,
			ProbeID: m.ProbeID, NotificationID: m.NotificationID, NotificationVersion: m.NotificationVersion,
			EventKind: m.EventKind, AvailableAt: m.AvailableAt.UTC(),
		},
		MonitorID: m.MonitorID, AssignmentGeneration: m.AssignmentGeneration,
		StreamID: m.StreamID, SourceSeq: m.SourceSeq, ConfigRevision: m.ConfigRevision,
		CheckStatus: domain.Status(m.CheckStatus), CheckOutput: m.CheckOutput, ObservedAt: m.ObservedAt.UTC(),
		IncidentStatus: m.IncidentStatus, StartedAt: m.StartedAt.UTC(), ResolvedAt: utcPtr(m.ResolvedAt),
		Status: m.Status, Attempt: m.Attempt, LeasedAt: utcPtr(m.LeasedAt), LeaseUntil: utcPtr(m.LeaseUntil), OutcomeAt: utcPtr(m.OutcomeAt), CreatedAt: m.CreatedAt.UTC(),
	}
	if m.LeaseToken != nil {
		out.LeaseToken = *m.LeaseToken
	}
	if m.ErrorCode != nil {
		out.ErrorCode = *m.ErrorCode
	}
	return out
}
