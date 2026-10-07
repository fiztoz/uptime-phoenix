package edge

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxTelemetryQueueBytes = 64 << 20

const maxDeliveryQueueBytes = 64 << 20

const maxTelemetryEventBytes = 64 << 10

// ErrQueueFull stops recording rather than silently deleting unacknowledged data.
// Configured retention must first persist a gap before it can make space.
var ErrQueueFull = errors.New("edge telemetry queue is full")

var _ ports.EdgeCheckRepository = (*Store)(nil)

type edgeStateRow struct {
	bun.BaseModel      `bun:"table:edge_regional_state"`
	MonitorID          int64 `bun:",pk"`
	Generation         int64 `bun:",pk"`
	Seq                int64
	ConfigRevision     int64
	Status             domain.Status
	DownCount          int
	ObservedAt         int64
	ReceivedAt         int64
	LastSuccessAt      *int64
	SourceAlertID      *string
	LastEnqueuedAt     *int64
	CurrentObservation []byte
}

type edgeIncidentRow struct {
	bun.BaseModel           `bun:"table:edge_alerts"`
	SourceAlertID           string `bun:",pk"`
	MonitorID               int64  `bun:"monitor_id,nullzero"`
	Generation              int64  `bun:"generation,nullzero"`
	Scope                   string
	SubjectKind             string
	ConditionKind           *string `bun:"condition_kind"`
	AckCommandID            string  `bun:"ack_command_id,nullzero"`
	AckActorDisplayName     string  `bun:"ack_actor_display_name,nullzero"`
	AckNote                 *string
	Status                  string
	TransitionVersion       int64
	StartedAt               int64
	ResolvedAt              *int64
	AckedAt                 *int64
	Reason                  string
	ConfigRevision          int64
	CertificateThreshold    int64  `bun:"certificate_threshold,nullzero"`
	CertificateNotAfter     *int64 `bun:"certificate_not_after"`
	EscalationPolicyID      *int64 `bun:"escalation_policy_id"`
	EscalationPolicyVersion *int64 `bun:"escalation_policy_version"`
	EscalationStatus        string `bun:"escalation_status,nullzero"`
	EscalationNextStep      *int64 `bun:"escalation_next_step"`
	EscalationNextRunAt     *int64 `bun:"escalation_next_run_at"`
}

func timeFromMicro(value *int64) *time.Time {
	if value == nil {
		return nil
	}
	at := time.UnixMicro(*value).UTC()
	return &at
}

func microFromTime(value *time.Time) *int64 {
	if value == nil {
		return nil
	}
	at := value.UTC().UnixMicro()
	return &at
}

func (row edgeIncidentRow) incident(probeID string) *domain.RegionalIncident {
	out := &domain.RegionalIncident{SourceAlertID: row.SourceAlertID, ProbeID: probeID, MonitorID: row.MonitorID, AssignmentGeneration: row.Generation, Scope: domain.IncidentScope(row.Scope), SubjectKind: row.SubjectKind, ConditionKind: edgeStringOrEmpty(row.ConditionKind), AckCommandID: row.AckCommandID, AckActorDisplayName: row.AckActorDisplayName, AckNote: row.AckNote, Status: row.Status, TransitionVersion: row.TransitionVersion, StartedAt: time.UnixMicro(row.StartedAt).UTC(), ResolvedAt: timeFromMicro(row.ResolvedAt), AckedAt: timeFromMicro(row.AckedAt), Reason: row.Reason, ConfigRevision: row.ConfigRevision, CertificateThreshold: row.CertificateThreshold, CertificateNotAfter: timeFromMicro(row.CertificateNotAfter), EscalationStatus: row.EscalationStatus, EscalationNextStep: row.EscalationNextStep, EscalationNextRunAt: timeFromMicro(row.EscalationNextRunAt)}
	if row.EscalationPolicyID != nil {
		out.EscalationPolicyID = *row.EscalationPolicyID
	}
	if row.EscalationPolicyVersion != nil {
		out.EscalationPolicyVersion = *row.EscalationPolicyVersion
	}
	return out
}

func readEdgeEvidence(ctx context.Context, db bun.IDB, i domain.EdgeIdentity, monitorID, generation int64) (domain.EdgeMonitorEvidence, error) {
	var row edgeStateRow
	err := db.NewSelect().Model(&row).Where("monitor_id = ? AND generation = ?", monitorID, generation).Scan(ctx)
	if errors.Is(storageError(ctx, err), ports.ErrNotFound) {
		return domain.EdgeMonitorEvidence{}, nil
	}
	if err != nil {
		return domain.EdgeMonitorEvidence{}, err
	}
	evidence := domain.EdgeMonitorEvidence{State: &domain.RegionalState{MonitorID: monitorID, ProbeID: i.ProbeID, AssignmentGeneration: generation, StreamID: i.StreamID, Seq: row.Seq, ConfigRevision: row.ConfigRevision, Status: row.Status, DownCount: row.DownCount, ObservedAt: time.UnixMicro(row.ObservedAt).UTC(), ReceivedAt: time.UnixMicro(row.ReceivedAt).UTC(), LastSuccessAt: timeFromMicro(row.LastSuccessAt)}, LastEnqueuedAt: timeFromMicro(row.LastEnqueuedAt)}
	if row.SourceAlertID != nil {
		var incident edgeIncidentRow
		if err := db.NewSelect().Model(&incident).Where("source_alert_id = ? AND monitor_id = ? AND generation = ?", *row.SourceAlertID, monitorID, generation).Scan(ctx); err != nil {
			return domain.EdgeMonitorEvidence{}, err
		}
		evidence.Incident = incident.incident(i.ProbeID)
	}
	certificate, certificateIncident, err := readEdgeCertState(ctx, db, monitorID, generation, i.ProbeID)
	if err != nil {
		return domain.EdgeMonitorEvidence{}, err
	}
	evidence.Certificate, evidence.CertificateIncident = certificate, certificateIncident
	conditions, err := readEdgeConditionStates(ctx, db, i.ProbeID, monitorID, generation)
	if err != nil {
		return domain.EdgeMonitorEvidence{}, err
	}
	evidence.Conditions = conditions
	return evidence, nil
}

// ReadEdgeEvidence returns a coherent state/incident/throttle view for evaluation.
func (s *Store) ReadEdgeEvidence(ctx context.Context, monitorID, generation int64) (domain.EdgeMonitorEvidence, error) {
	if monitorID <= 0 || generation <= 0 {
		return domain.EdgeMonitorEvidence{}, domain.ErrValidation
	}
	var result domain.EdgeMonitorEvidence
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		i, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		result, err = readEdgeEvidence(ctx, tx, i, monitorID, generation)
		return err
	})
	if err != nil {
		return domain.EdgeMonitorEvidence{}, storageError(ctx, err)
	}
	return result, nil
}

// CommitEdgeCheck persists source state, exact events and delivery work together.
// Provider I/O is impossible here; only the injected pure encoder is invoked.
func (s *Store) CommitEdgeCheck(ctx context.Context, record domain.EdgeCheckRecord) (domain.RegionalObservation, error) {
	o := record.Observation
	if s.telemetry == nil || !domain.ValidTLSObservation(o.TLS) || record.ExpectedStateSeq < 0 || record.ExpectedIncidentVersion < 0 || record.ExpectedCertificateVersion < 0 || o.MonitorID <= 0 || o.AssignmentGeneration <= 0 || o.ConfigRevision <= 0 || o.Seq != 0 || o.ObservedAt.IsZero() || o.ReceivedAt.IsZero() || o.Status < domain.StatusDown || o.Status > domain.StatusMaintenance || o.RawStatus != domain.StatusUp && o.RawStatus != domain.StatusDown && o.RawStatus != domain.StatusMaintenance || len(record.DeliveryIntents) > 1000 || len(record.Conditions) > 2 {
		return domain.RegionalObservation{}, domain.ErrValidation
	}
	for _, work := range record.Conditions {
		if work.ExpectedVersion < 0 {
			return domain.RegionalObservation{}, domain.ErrValidation
		}
	}
	o.ObservedAt, o.ReceivedAt = o.ObservedAt.UTC(), o.ReceivedAt.UTC()
	var committed domain.RegionalObservation
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if o.ProbeID != i.ProbeID || o.StreamID != i.StreamID || o.ConfigRevision != i.ConfigRevision {
			return ports.ErrConflict
		}
		var assignment domain.EdgeAssignmentIdentity
		if err := tx.NewRaw("SELECT monitor_id, generation, active FROM edge_assignments WHERE monitor_id = ? AND revision = ?", o.MonitorID, i.ConfigRevision).Scan(ctx, &assignment); err != nil {
			return err
		}
		if !assignment.Active || assignment.Generation != o.AssignmentGeneration {
			return ports.ErrConflict
		}
		before, err := readEdgeEvidence(ctx, tx, i, o.MonitorID, o.AssignmentGeneration)
		if err != nil {
			return err
		}
		var previousSeq int64
		if before.State != nil {
			previousSeq = before.State.Seq
		}
		var incidentVersion int64
		if before.Incident != nil {
			incidentVersion = before.Incident.TransitionVersion
		}
		if previousSeq != record.ExpectedStateSeq || incidentVersion != record.ExpectedIncidentVersion {
			return ports.ErrStaleLocalState
		}
		var certificateVersion int64
		if before.Certificate != nil {
			certificateVersion = before.Certificate.Version
		}
		if certificateVersion != record.ExpectedCertificateVersion {
			return ports.ErrStaleLocalState
		}
		eventCount := int64(1)
		if record.Incident != nil {
			eventCount++
		}
		if record.Certificate != nil {
			// One evaluation can retire at most one superseded threshold incident and
			// open at most one replacement. Anything larger is a caller defect.
			if len(record.Certificate.Transitions) == 0 || len(record.Certificate.Transitions) > 2 {
				return domain.ErrValidation
			}
			eventCount += int64(len(record.Certificate.Transitions))
		}
		for _, work := range record.Conditions {
			if !work.Remove && work.Transition != nil {
				eventCount++
			}
			if work.Alert != nil && work.Alert.Incident != nil {
				eventCount++
			}
		}
		if i.LastCreatedSeq > math.MaxInt64-eventCount {
			return ports.ErrConflict
		}
		o.Seq = i.LastCreatedSeq + 1
		observationBytes, err := s.telemetry.EncodeObservation(o)
		if err != nil {
			return domain.ErrValidation
		}
		if err := s.appendTelemetry(ctx, tx, o.Seq, "observation", o.ObservedAt, observationBytes); err != nil {
			return err
		}
		incident := before.Incident
		if record.Incident != nil {
			if err := saveEdgeIncident(ctx, tx, o, before.Incident, *record.Incident); err != nil {
				return err
			}
			incident = record.Incident
			payload, err := s.telemetry.EncodeIncident(o.Seq+1, o.ObservedAt, *incident)
			if err != nil {
				return domain.ErrValidation
			}
			if err := s.appendTelemetry(ctx, tx, o.Seq+1, "alert.transition", o.ObservedAt, payload); err != nil {
				return err
			}
		}
		nextSeq := o.Seq + 1
		if record.Incident != nil {
			nextSeq++
		}
		certSeq, err := s.applyEdgeCertAlertWork(ctx, tx, o, before, record.Certificate, nextSeq)
		if err != nil {
			return err
		}
		if _, err := s.applyEdgeConditionWork(ctx, tx, o, before.Conditions, record.Conditions, certSeq); err != nil {
			return err
		}
		state := edgeStateRow{CurrentObservation: observationBytes, MonitorID: o.MonitorID, Generation: o.AssignmentGeneration, Seq: o.Seq, ConfigRevision: o.ConfigRevision, Status: o.Status, DownCount: o.DownCount, ObservedAt: o.ObservedAt.UnixMicro(), ReceivedAt: o.ReceivedAt.UnixMicro(), LastEnqueuedAt: microFromTime(before.LastEnqueuedAt)}
		if before.State != nil {
			state.LastSuccessAt = microFromTime(before.State.LastSuccessAt)
		}
		if o.Status == domain.StatusUp {
			state.LastSuccessAt = microFromTime(&o.ObservedAt)
		}
		if incident != nil {
			state.SourceAlertID = &incident.SourceAlertID
		}
		for _, intent := range record.DeliveryIntents {
			if err := insertEdgeIntent(ctx, tx, o, incident, intent); err != nil {
				return err
			}
		}
		if len(record.DeliveryIntents) > 0 {
			at := o.ObservedAt
			if before.LastEnqueuedAt != nil && before.LastEnqueuedAt.After(at) {
				at = *before.LastEnqueuedAt
			}
			state.LastEnqueuedAt = microFromTime(&at)
		}
		if _, err := tx.NewInsert().Model(&state).On("CONFLICT (monitor_id, generation) DO UPDATE").Set("seq = EXCLUDED.seq").Set("config_revision = EXCLUDED.config_revision").Set("status = EXCLUDED.status").Set("down_count = EXCLUDED.down_count").Set("observed_at = EXCLUDED.observed_at").Set("received_at = EXCLUDED.received_at").Set("last_success_at = EXCLUDED.last_success_at").Set("source_alert_id = EXCLUDED.source_alert_id").Set("last_enqueued_at = EXCLUDED.last_enqueued_at").Set("current_observation = EXCLUDED.current_observation").Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET last_created_seq = ? WHERE id = 1", i.LastCreatedSeq+eventCount); err != nil {
			return err
		}
		committed = o
		return nil
	})
	if err != nil {
		return domain.RegionalObservation{}, err
	}
	return committed, nil
}

// sameRecordedRawCondition proves the evaluated work describes the same raw
// checker measurement the immutable observation payload carries for its kind.
func sameRecordedRawCondition(recorded []domain.ConditionObservation, raw domain.ConditionObservation) bool {
	for _, condition := range recorded {
		if condition.Kind != raw.Kind {
			continue
		}
		return condition.State == raw.State && condition.Message == raw.Message &&
			condition.Unit == raw.Unit && condition.Resource == raw.Resource && condition.Scope == raw.Scope && condition.Source == raw.Source &&
			condition.ObservedAt.UTC().Truncate(time.Microsecond).Equal(raw.ObservedAt.UTC().Truncate(time.Microsecond)) &&
			condition.StaleAfter.UTC().Truncate(time.Microsecond).Equal(raw.StaleAfter.UTC().Truncate(time.Microsecond)) &&
			sameOptionalFloat(condition.Used, raw.Used) && sameOptionalFloat(condition.Limit, raw.Limit) &&
			sameOptionalFloat(condition.Percent, raw.Percent) && sameOptionalFloat(condition.Threshold, raw.Threshold)
	}
	return false
}

func sameOptionalFloat(a, b *float64) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	return a == nil || *a == *b
}

func (s *Store) appendTelemetry(ctx context.Context, tx bun.Tx, seq int64, kind string, at time.Time, payload []byte) error {
	if len(payload) == 0 || len(payload) > maxTelemetryEventBytes {
		return domain.ErrValidation
	}
	var size int64
	if err := tx.NewRaw("SELECT COALESCE(SUM(length(payload)), 0) FROM edge_telemetry_outbox").Scan(ctx, &size); err != nil {
		return err
	}
	var leased int64
	if err := tx.NewRaw("SELECT COUNT(*) FROM edge_delivery_outbox WHERE status = 'leased'").Scan(ctx, &leased); err != nil {
		return err
	}
	if kind == "delivery.result" {
		// This attempt consumes its reserved outcome space in the same transaction
		// that releases its delivery lease. Recording cannot consume that reserve.
		leased--
		if leased < 0 {
			return domain.ErrValidation
		}
	}
	if s.retention.MaxBytes > 0 {
		if err := s.retainTelemetry(ctx, tx, time.Now().UTC(), kind, int64(len(payload))+queueRowBytes, leased*(maxTelemetryEventBytes+queueRowBytes)); err != nil {
			return err
		}
	} else if size > maxTelemetryQueueBytes-int64(len(payload))-leased*maxTelemetryEventBytes {
		return ErrQueueFull
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO edge_telemetry_outbox (seq, kind, observed_at, payload) VALUES (?, ?, ?, ?)", seq, kind, at.UTC().UnixMicro(), payload)
	return err
}

func saveEdgeIncident(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, prior *domain.RegionalIncident, inc domain.RegionalIncident) error {
	if !domain.ValidHubID(inc.SourceAlertID) || inc.ProbeID != o.ProbeID || inc.MonitorID != o.MonitorID || inc.AssignmentGeneration != o.AssignmentGeneration || inc.ConfigRevision != o.ConfigRevision || inc.Scope != domain.IncidentScopeRegional || inc.SubjectKind != domain.IncidentSubjectAvailability || inc.StartedAt.IsZero() || !domain.ValidIncidentEscalation(&inc) {
		return domain.ErrValidation
	}
	row := newEdgeIncidentRow(inc)
	if prior == nil || prior.Status == domain.AlertStatusResolved {
		if inc.Status != domain.AlertStatusFiring || inc.TransitionVersion != 1 || inc.ResolvedAt != nil || inc.AckedAt != nil || inc.AckCommandID != "" || inc.AckActorDisplayName != "" || inc.AckNote != nil || o.Status != domain.StatusDown || inc.EscalationStatus != "" && inc.EscalationStatus != domain.EscalationStatePending {
			return domain.ErrValidation
		}
		_, err := tx.NewInsert().Model(&row).Exec(ctx)
		return err
	}
	if !sameIncidentAcknowledgement(inc, *prior) || !sameSettledEscalation(*prior, inc) || (prior.Status != domain.AlertStatusFiring && prior.Status != domain.AlertStatusAcked) || inc.SourceAlertID != prior.SourceAlertID || inc.StartedAt.UTC().UnixMicro() != prior.StartedAt.UTC().UnixMicro() || prior.TransitionVersion == math.MaxInt64 || inc.TransitionVersion != prior.TransitionVersion+1 || inc.Status != domain.AlertStatusResolved || inc.ResolvedAt == nil || o.Status != domain.StatusUp {
		return domain.ErrValidation
	}
	_, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
	return err
}

// sameSettledEscalation keeps the policy that armed the ladder and requires
// recovery to cancel pending work instead of leaving a step due.
func sameSettledEscalation(prior, next domain.RegionalIncident) bool {
	if prior.EscalationPolicyID != next.EscalationPolicyID {
		return false
	}
	switch prior.EscalationStatus {
	case "":
		return next.EscalationStatus == ""
	case domain.EscalationStatePending, domain.EscalationStateCanceled:
		return next.EscalationStatus == domain.EscalationStateCanceled && next.EscalationNextStep == nil && next.EscalationNextRunAt == nil
	case domain.EscalationStateDone:
		return next.EscalationStatus == domain.EscalationStateDone && next.EscalationNextStep == nil && next.EscalationNextRunAt == nil
	default:
		return false
	}
}

func insertEdgeIntent(ctx context.Context, tx bun.Tx, o domain.RegionalObservation, inc *domain.RegionalIncident, intent domain.DeliveryIntent) error {
	if inc == nil || !domain.ValidHubID(intent.DeliveryID) || intent.ProbeID != o.ProbeID || intent.SourceAlertID != inc.SourceAlertID || intent.SourceTransitionVersion != inc.TransitionVersion || intent.NotificationID <= 0 || intent.NotificationVersion <= 0 || intent.NotificationVersion > o.ConfigRevision || intent.AvailableAt.IsZero() || intent.EscalationPolicyID != 0 || intent.EscalationStep != 0 || o.Status != domain.StatusUp && o.Status != domain.StatusDown {
		return domain.ErrValidation
	}
	if intent.EventKind != domain.DeliveryEventStatusChange && intent.EventKind != domain.DeliveryEventIncidentSummary || inc.Status != domain.AlertStatusFiring && inc.Status != domain.AlertStatusResolved {
		return domain.ErrValidation
	}
	if o.Status == domain.StatusDown && inc.Status != domain.AlertStatusFiring || o.Status == domain.StatusUp && inc.Status != domain.AlertStatusResolved {
		return domain.ErrValidation
	}
	if inc.Status == domain.AlertStatusResolved {
		// Decide per channel inside the recovery transaction, against durable
		// outcomes rather than an earlier service snapshot. Unsent outages need
		// one summary, not an ordinary recovery for a DOWN nobody received.
		sent, err := tx.NewSelect().Model((*edgeDeliveryRow)(nil)).
			Where("source_alert_id = ? AND notification_id = ? AND check_status = ? AND status = ?", inc.SourceAlertID, intent.NotificationID, domain.StatusDown, domain.DeliveryStatusSent).Exists(ctx)
		if err != nil {
			return err
		}
		if !sent {
			intent.EventKind = domain.DeliveryEventIncidentSummary
		}
	}
	return insertEdgeQueuedDelivery(ctx, tx, domain.QueuedDelivery{DeliveryIntent: intent, MonitorID: o.MonitorID, AssignmentGeneration: o.AssignmentGeneration, StreamID: o.StreamID, SourceSeq: o.Seq, ConfigRevision: o.ConfigRevision, CheckStatus: o.Status, CheckOutput: o.Message, ObservedAt: o.ObservedAt, IncidentStatus: inc.Status, StartedAt: inc.StartedAt, ResolvedAt: inc.ResolvedAt, CreatedAt: o.ReceivedAt})
}

func newEdgeIncidentRow(inc domain.RegionalIncident) edgeIncidentRow {
	row := edgeIncidentRow{SourceAlertID: inc.SourceAlertID, MonitorID: inc.MonitorID, Generation: inc.AssignmentGeneration, Scope: string(inc.Scope), SubjectKind: inc.SubjectKind, Status: inc.Status, TransitionVersion: inc.TransitionVersion, StartedAt: inc.StartedAt.UTC().UnixMicro(), ResolvedAt: microFromTime(inc.ResolvedAt), AckedAt: microFromTime(inc.AckedAt), AckCommandID: inc.AckCommandID, AckActorDisplayName: inc.AckActorDisplayName, AckNote: inc.AckNote, Reason: inc.Reason, ConfigRevision: inc.ConfigRevision, CertificateThreshold: inc.CertificateThreshold, CertificateNotAfter: microFromTime(inc.CertificateNotAfter), EscalationStatus: inc.EscalationStatus, EscalationNextStep: inc.EscalationNextStep, EscalationNextRunAt: microFromTime(inc.EscalationNextRunAt)}
	if inc.ConditionKind != "" {
		row.ConditionKind = edgeOptionalString(inc.ConditionKind)
	}
	if inc.EscalationPolicyID > 0 {
		id := inc.EscalationPolicyID
		row.EscalationPolicyID = &id
	}
	if inc.EscalationPolicyVersion > 0 {
		version := inc.EscalationPolicyVersion
		row.EscalationPolicyVersion = &version
	}
	return row
}

// Source checks may preserve a committed acknowledgement, never create or edit it.
func sameIncidentAcknowledgement(a, b domain.RegionalIncident) bool {
	if a.AckCommandID != b.AckCommandID || a.AckActorDisplayName != b.AckActorDisplayName || (a.AckedAt == nil) != (b.AckedAt == nil) || (a.AckNote == nil) != (b.AckNote == nil) {
		return false
	}
	if a.AckedAt != nil && a.AckedAt.UTC().UnixMicro() != b.AckedAt.UTC().UnixMicro() {
		return false
	}
	return a.AckNote == nil || *a.AckNote == *b.AckNote
}
