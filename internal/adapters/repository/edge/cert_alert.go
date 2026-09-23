package edge

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// edgeCertAlertRow is the durable, source-owned certificate paging cursor. The
// storage CHECK enforces the same pairing invariant the domain validator does: a
// delivered threshold always names the exact expiry it was delivered for and the
// one open incident that represents it.
type edgeCertAlertRow struct {
	bun.BaseModel  `bun:"table:edge_cert_alert_state"`
	MonitorID      int64  `bun:",pk"`
	Generation     int64  `bun:",pk"`
	ConfigRevision int64  `bun:"config_revision"`
	AlertNotAfter  *int64 `bun:"alert_not_after"`
	SourceAlertID  *string
	UpdatedAt      int64
	AlertThreshold int
	Version        int64
}

func (row edgeCertAlertRow) state() *domain.EdgeCertAlertState {
	return &domain.EdgeCertAlertState{
		MonitorID: row.MonitorID, AssignmentGeneration: row.Generation, ConfigRevision: row.ConfigRevision,
		AlertNotAfter: timeFromMicro(row.AlertNotAfter), UpdatedAt: time.UnixMicro(row.UpdatedAt).UTC(),
		SourceAlertID: edgeStringOrEmpty(row.SourceAlertID), AlertThreshold: row.AlertThreshold, Version: row.Version,
	}
}

func edgeStringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func edgeOptionalString(value string) *string {
	if value == "" {
		return nil
	}
	_copy := value
	return &_copy
}

// readEdgeCertState returns the assignment cursor and its open certificate
// incident, or a nil pair when the assignment has never paged a certificate.
func readEdgeCertState(ctx context.Context, db bun.IDB, monitorID, generation int64, probeID string) (*domain.EdgeCertAlertState, *domain.RegionalIncident, error) {
	var row edgeCertAlertRow
	err := db.NewSelect().Model(&row).Where("monitor_id = ? AND generation = ?", monitorID, generation).Scan(ctx)
	if errors.Is(storageError(ctx, err), ports.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	state := row.state()
	if !domain.ValidEdgeCertAlertState(state) {
		return nil, nil, fmt.Errorf("stored certificate cursor is inconsistent: %w", ErrStorage)
	}
	if state.SourceAlertID == "" {
		return state, nil, nil
	}
	var incident edgeIncidentRow
	if err := db.NewSelect().Model(&incident).Where("source_alert_id = ? AND monitor_id = ? AND generation = ?", state.SourceAlertID, monitorID, generation).Scan(ctx); err != nil {
		return nil, nil, err
	}
	open := incident.incident(probeID)
	if open.SubjectKind != domain.IncidentSubjectCertificate || !domain.ValidCertificateIncident(open) {
		return nil, nil, fmt.Errorf("certificate cursor references a non-certificate incident: %w", ErrStorage)
	}
	return state, open, nil
}

// applyEdgeCertAlertWork stores the evaluated certificate lifecycle, emits one
// ordered alert.transition event per transition, records the firing alert's
// delivery work and advances the fenced cursor. It returns the next free
// sequence number. Provider I/O is impossible here: only the pure encoder runs.
func (s *Store) applyEdgeCertAlertWork(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, before domain.EdgeMonitorEvidence, work *domain.EdgeCertAlertWork, seq int64) (int64, error) {
	if work == nil {
		return seq, nil
	}
	if len(work.Transitions) == 0 {
		return seq, domain.ErrValidation
	}
	var expected int64
	if before.Certificate != nil {
		expected = before.Certificate.Version
	}
	if work.Cursor.Version != expected+1 || work.Cursor.MonitorID != o.MonitorID || work.Cursor.AssignmentGeneration != o.AssignmentGeneration ||
		work.Cursor.ConfigRevision != o.ConfigRevision || !domain.ValidEdgeCertAlertState(&work.Cursor) {
		return seq, domain.ErrValidation
	}
	prior := before.CertificateIncident
	var fired *domain.RegionalIncident
	for _, transition := range work.Transitions {
		subject := transition
		if err := saveEdgeCertificateIncident(ctx, tx, o, prior, subject); err != nil {
			return seq, err
		}
		if prior == nil || prior.SourceAlertID == subject.SourceAlertID {
			prior = &subject
		}
		if subject.Status == domain.AlertStatusFiring {
			value := subject
			fired = &value
		}
		payload, err := s.telemetry.EncodeIncident(seq, o.ObservedAt, subject)
		if err != nil {
			return seq, err
		}
		if err := s.appendTelemetry(ctx, tx, seq, "alert.transition", o.ObservedAt, payload); err != nil {
			return seq, err
		}
		seq++
	}
	if fired != nil {
		for _, intent := range work.Intents {
			if err := insertEdgeCertificateIntent(ctx, tx, o, *fired, intent, work.Certificate); err != nil {
				return seq, err
			}
		}
	} else if len(work.Intents) > 0 {
		return seq, domain.ErrValidation
	}
	if _, err := tx.NewInsert().Model(&edgeCertAlertRow{
		MonitorID: work.Cursor.MonitorID, Generation: work.Cursor.AssignmentGeneration, ConfigRevision: work.Cursor.ConfigRevision,
		AlertNotAfter: microFromTime(work.Cursor.AlertNotAfter), SourceAlertID: edgeOptionalString(work.Cursor.SourceAlertID),
		UpdatedAt: work.Cursor.UpdatedAt.UTC().UnixMicro(), AlertThreshold: work.Cursor.AlertThreshold, Version: work.Cursor.Version,
	}).On("CONFLICT (monitor_id, generation) DO UPDATE").
		Set("config_revision = EXCLUDED.config_revision").Set("alert_threshold = EXCLUDED.alert_threshold").
		Set("alert_not_after = EXCLUDED.alert_not_after").Set("source_alert_id = EXCLUDED.source_alert_id").
		Set("updated_at = EXCLUDED.updated_at").Set("version = EXCLUDED.version").Exec(ctx); err != nil {
		return seq, fmt.Errorf("advance certificate cursor: %w", err)
	}
	return seq, nil
}

// saveEdgeCertificateIncident writes one certificate incident version. A firing
// row is always a new identity at version one; a resolved row may only advance
// the stored incident this cursor points at, and must carry that incident's
// immutable subject forward rather than restate the replacement certificate.
func saveEdgeCertificateIncident(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, prior *domain.RegionalIncident, inc domain.RegionalIncident) error {
	if !domain.ValidHubID(inc.SourceAlertID) || inc.ProbeID != o.ProbeID || inc.MonitorID != o.MonitorID ||
		inc.AssignmentGeneration != o.AssignmentGeneration || inc.ConfigRevision != o.ConfigRevision ||
		!domain.ValidCertificateIncident(&inc) || inc.StartedAt.IsZero() {
		return domain.ErrValidation
	}
	row := newEdgeIncidentRow(inc)
	switch inc.Status {
	case domain.AlertStatusFiring:
		if inc.TransitionVersion != 1 || inc.ResolvedAt != nil || inc.AckedAt != nil || inc.AckCommandID != "" || inc.AckActorDisplayName != "" || inc.AckNote != nil {
			return domain.ErrValidation
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return fmt.Errorf("insert certificate incident: %w", err)
		}
		return nil
	case domain.AlertStatusResolved:
	default:
		return domain.ErrValidation
	}
	if prior == nil || prior.SourceAlertID != inc.SourceAlertID || prior.StartedAt.UTC().UnixMicro() != inc.StartedAt.UTC().UnixMicro() ||
		prior.TransitionVersion == math.MaxInt64 || inc.TransitionVersion != prior.TransitionVersion+1 ||
		inc.CertificateThreshold != prior.CertificateThreshold || !sameStoredCertificateExpiry(inc.CertificateNotAfter, prior.CertificateNotAfter) ||
		!sameIncidentAcknowledgement(inc, *prior) || prior.Status == domain.AlertStatusResolved || inc.ResolvedAt == nil {
		return domain.ErrValidation
	}
	if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
		return fmt.Errorf("advance certificate incident: %w", err)
	}
	return nil
}

// sameStoredCertificateExpiry requires the same instant on both sides; a missing
// expiry never matches, because a certificate subject always carries one.
func sameStoredCertificateExpiry(a, b *time.Time) bool {
	return a != nil && b != nil && a.UTC().Equal(b.UTC())
}

// insertEdgeCertificateIntent records durable source work for one fired
// certificate threshold. The rendered snapshot is stored with the row, so a
// retry after a restart delivers the threshold it was committed for instead of
// re-deriving one from a later clock or a pruned observation.
func insertEdgeCertificateIntent(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, inc domain.RegionalIncident, intent domain.DeliveryIntent, content *domain.EdgeCertAlertContent) error {
	if !domain.ValidEdgeCertAlertContent(content) || inc.Status != domain.AlertStatusFiring || content.Threshold != int(inc.CertificateThreshold) ||
		!content.NotAfter.UTC().Equal(inc.CertificateNotAfter.UTC()) || inc.TransitionVersion != 1 {
		return domain.ErrValidation
	}
	if err := validateEdgeDeliveryIdentity(o, &inc, intent); err != nil {
		return err
	}
	if intent.EventKind != domain.DeliveryEventCertificateExpiry {
		return domain.ErrValidation
	}
	return insertEdgeQueuedDelivery(ctx, tx, domain.QueuedDelivery{
		DeliveryIntent: intent, MonitorID: o.MonitorID, AssignmentGeneration: o.AssignmentGeneration, StreamID: o.StreamID,
		SourceSeq: o.Seq, ConfigRevision: o.ConfigRevision, CheckStatus: o.Status, CheckOutput: content.Message,
		ObservedAt: o.ObservedAt, IncidentStatus: inc.Status, StartedAt: inc.StartedAt, ResolvedAt: inc.ResolvedAt,
		CreatedAt: o.ReceivedAt, Certificate: content,
	})
}

// validateEdgeDeliveryIdentity holds the checks every source intent shares,
// independent of whether it carries availability or auxiliary content.
func validateEdgeDeliveryIdentity(o domain.RegionalObservation, inc *domain.RegionalIncident, intent domain.DeliveryIntent) error {
	if inc == nil || !domain.ValidHubID(intent.DeliveryID) || intent.ProbeID != o.ProbeID || intent.SourceAlertID != inc.SourceAlertID ||
		intent.SourceTransitionVersion != inc.TransitionVersion || intent.NotificationID <= 0 || intent.NotificationVersion <= 0 ||
		intent.NotificationVersion > o.ConfigRevision || intent.AvailableAt.IsZero() || intent.EscalationPolicyID != 0 || intent.EscalationStep != 0 {
		return domain.ErrValidation
	}
	if o.Status != domain.StatusUp && o.Status != domain.StatusDown {
		return domain.ErrValidation
	}
	return nil
}
