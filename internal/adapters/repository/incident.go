package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeIncidentModel struct {
	bun.BaseModel           `bun:"table:probe_incidents,alias:inc"`
	HubIncidentID           int64      `bun:"hub_incident_id,pk,autoincrement"`
	SourceAlertID           string     `bun:"source_alert_id,notnull"`
	Scope                   string     `bun:"scope,notnull"`
	MonitorID               *int64     `bun:"monitor_id"`
	ProbeID                 string     `bun:"probe_id,notnull"`
	AssignmentGeneration    *int64     `bun:"assignment_generation"`
	Status                  string     `bun:"status,notnull"`
	TransitionVersion       int64      `bun:"transition_version,notnull"`
	StartedAt               time.Time  `bun:"started_at,notnull"`
	ResolvedAt              *time.Time `bun:"resolved_at"`
	AckedAt                 *time.Time `bun:"acked_at"`
	Reason                  string     `bun:"reason,notnull"`
	ConfigRevision          int64      `bun:"config_revision,notnull"`
	SubjectKind             string     `bun:"subject_kind,notnull"`
	ConditionKind           *string    `bun:"condition_kind"`
	CertificateThreshold    *int       `bun:"certificate_threshold"`
	AckCommandID            *string    `bun:"ack_command_id"`
	AckActorDisplayName     *string    `bun:"ack_actor_display_name"`
	AckNote                 *string    `bun:"ack_note"`
	EscalationPolicyID      *int64     `bun:"escalation_policy_id"`
	EscalationPolicyVersion *int64     `bun:"escalation_policy_version"`
	EscalationStatus        *string    `bun:"escalation_status"`
	EscalationNextStep      *int64     `bun:"escalation_next_step"`
	EscalationNextRunAt     *time.Time `bun:"escalation_next_run_at"`
	CreatedAt               time.Time  `bun:"created_at,notnull"`
	UpdatedAt               time.Time  `bun:"updated_at,notnull"`
}

type probeDeliveryModel struct {
	bun.BaseModel           `bun:"table:probe_delivery_events,alias:del"`
	DeliveryID              string    `bun:"delivery_id,pk"`
	SourceAlertID           string    `bun:"source_alert_id,notnull"`
	SourceTransitionVersion int64     `bun:"source_transition_version,notnull"`
	ProbeID                 string    `bun:"probe_id,notnull"`
	NotificationID          int64     `bun:"notification_id,notnull"`
	NotificationVersion     int64     `bun:"notification_version,notnull"`
	EventKind               string    `bun:"event_kind,notnull"`
	Attempt                 int64     `bun:"attempt,notnull"`
	Status                  string    `bun:"status,notnull"`
	ErrorCode               *string   `bun:"error_code"`
	ObservedAt              time.Time `bun:"observed_at,notnull"`
	CreatedAt               time.Time `bun:"created_at,notnull"`
	UpdatedAt               time.Time `bun:"updated_at,notnull"`
}

var (
	_ ports.ProbeIncidentRepository = (*RegionalCommitStore)(nil)
	_ ports.ProbeDeliveryRepository = (*RegionalCommitStore)(nil)
)

// PutIncident inserts or advances a source-owned incident. Same-version retries are no-ops.
func (r *RegionalCommitStore) PutIncident(ctx context.Context, incident *domain.RegionalIncident) error {
	if incident == nil {
		return fmt.Errorf("incident: %w", domain.ErrValidation)
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return putIncidentTx(ctx, tx, incident)
	})
}

// GetIncident returns one incident by source identity.
func (r *RegionalCommitStore) GetIncident(ctx context.Context, sourceAlertID string) (*domain.RegionalIncident, error) {
	id, err := canonicalIdentity("source_alert_id", sourceAlertID)
	if err != nil {
		return nil, err
	}
	m := new(probeIncidentModel)
	if err := r.db.NewSelect().Model(m).Where("source_alert_id = ?", id).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get incident: %w", probeRegistryError(err))
	}
	out := incidentFromModel(m)
	return &out, nil
}

// ListIncidentsByMonitor returns incidents for one monitor, ordered by start then hub id.
func (r *RegionalCommitStore) ListIncidentsByMonitor(ctx context.Context, monitorID int64) ([]domain.RegionalIncident, error) {
	if monitorID < 1 {
		return nil, fmt.Errorf("invalid monitor ID: %w", domain.ErrValidation)
	}
	var rows []probeIncidentModel
	if err := r.db.NewSelect().Model(&rows).
		Where("monitor_id = ?", monitorID).
		OrderExpr("started_at ASC, hub_incident_id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list incidents: %w", err)
	}
	out := make([]domain.RegionalIncident, 0, len(rows))
	for i := range rows {
		out = append(out, incidentFromModel(&rows[i]))
	}
	return out, nil
}

// PutDelivery records a source delivery outcome. It never sends a notification.
func (r *RegionalCommitStore) PutDelivery(ctx context.Context, delivery *domain.RegionalDelivery) error {
	if delivery == nil {
		return fmt.Errorf("delivery: %w", domain.ErrValidation)
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return putDeliveryTx(ctx, tx, delivery)
	})
}

// GetDelivery returns one delivery outcome.
func (r *RegionalCommitStore) GetDelivery(ctx context.Context, deliveryID string) (*domain.RegionalDelivery, error) {
	id, err := canonicalIdentity("delivery_id", deliveryID)
	if err != nil {
		return nil, err
	}
	m := new(probeDeliveryModel)
	if err := r.db.NewSelect().Model(m).Where("delivery_id = ?", id).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get delivery: %w", probeRegistryError(err))
	}
	out := deliveryFromModel(m)
	return &out, nil
}

// ListDeliveriesByIncident returns deliveries for one source incident.
func (r *RegionalCommitStore) ListDeliveriesByIncident(ctx context.Context, sourceAlertID string) ([]domain.RegionalDelivery, error) {
	id, err := canonicalIdentity("source_alert_id", sourceAlertID)
	if err != nil {
		return nil, err
	}
	var rows []probeDeliveryModel
	if err := r.db.NewSelect().Model(&rows).
		Where("source_alert_id = ?", id).
		OrderExpr("observed_at ASC, delivery_id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	out := make([]domain.RegionalDelivery, 0, len(rows))
	for i := range rows {
		out = append(out, deliveryFromModel(&rows[i]))
	}
	return out, nil
}

func putIncidentTx(ctx context.Context, tx bun.Tx, incident *domain.RegionalIncident) error {
	if err := validateIncident(incident); err != nil {
		return err
	}
	now := time.Now().UTC()
	row := incidentModel(*incident)
	row.CreatedAt = now
	row.UpdatedAt = now
	existing := new(probeIncidentModel)
	err := tx.NewSelect().Model(existing).Where("source_alert_id = ?", row.SourceAlertID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return fmt.Errorf("insert incident: %w", probeRegistryError(err))
		}
		if row.HubIncidentID == 0 {
			if err := tx.NewSelect().Table("probe_incidents").Column("hub_incident_id").
				Where("source_alert_id = ?", row.SourceAlertID).Scan(ctx, &row.HubIncidentID); err != nil {
				return fmt.Errorf("load hub incident id: %w", err)
			}
		}
		incident.HubIncidentID = row.HubIncidentID
		return nil
	}
	if err != nil {
		return err
	}
	if err := incidentIdentityConflict(existing, &row); err != nil {
		return err
	}
	if row.TransitionVersion < existing.TransitionVersion {
		return ports.ErrConflict
	}
	if row.TransitionVersion == existing.TransitionVersion {
		if !sameIncidentSnapshot(existing, &row) {
			return ports.ErrConflict
		}
		incident.HubIncidentID = existing.HubIncidentID
		return nil
	}
	if existing.Status == domain.AlertStatusResolved && row.Status != domain.AlertStatusResolved {
		return ports.ErrConflict
	}
	row.HubIncidentID = existing.HubIncidentID
	row.CreatedAt = existing.CreatedAt
	if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
		return fmt.Errorf("update incident: %w", err)
	}
	incident.HubIncidentID = existing.HubIncidentID
	return nil
}

func putDeliveryTx(ctx context.Context, tx bun.Tx, delivery *domain.RegionalDelivery) error {
	if err := validateDelivery(delivery); err != nil {
		return err
	}
	incident := new(probeIncidentModel)
	if err := tx.NewSelect().Model(incident).Where("source_alert_id = ?", delivery.SourceAlertID).Scan(ctx); err != nil {
		return fmt.Errorf("delivery incident: %w", probeRegistryError(err))
	}
	if delivery.SourceTransitionVersion > incident.TransitionVersion || delivery.ProbeID != incident.ProbeID {
		return ports.ErrConflict
	}
	now := time.Now().UTC()
	row := deliveryModel(*delivery)
	row.CreatedAt = now
	row.UpdatedAt = now
	existing := new(probeDeliveryModel)
	err := tx.NewSelect().Model(existing).Where("delivery_id = ?", row.DeliveryID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return fmt.Errorf("insert delivery: %w", probeRegistryError(err))
		}
		return nil
	}
	if err != nil {
		return err
	}
	if existing.SourceAlertID != row.SourceAlertID || existing.SourceTransitionVersion != row.SourceTransitionVersion ||
		existing.ProbeID != row.ProbeID || existing.NotificationID != row.NotificationID ||
		existing.NotificationVersion != row.NotificationVersion || existing.EventKind != row.EventKind {
		return ports.ErrConflict
	}
	if row.Attempt < existing.Attempt {
		return ports.ErrConflict
	}
	row.CreatedAt = existing.CreatedAt
	if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
		return fmt.Errorf("update delivery: %w", err)
	}
	return nil
}

func validateIncident(incident *domain.RegionalIncident) error {
	id, err := canonicalIdentity("source_alert_id", incident.SourceAlertID)
	if err != nil {
		return err
	}
	incident.SourceAlertID = id
	if incident.ProbeID == "" {
		return fmt.Errorf("incident probe: %w", domain.ErrValidation)
	}
	switch incident.Scope {
	case domain.IncidentScopeRegional:
		if incident.MonitorID < 1 || incident.AssignmentGeneration < 1 {
			return fmt.Errorf("regional incident identity: %w", domain.ErrValidation)
		}
		if incident.SubjectKind == domain.IncidentSubjectWatchdog {
			return fmt.Errorf("watchdog subject: %w", domain.ErrValidation)
		}
	case domain.IncidentScopeProbeConnection:
		if incident.MonitorID != 0 || incident.AssignmentGeneration != 0 || incident.SubjectKind != domain.IncidentSubjectWatchdog {
			return fmt.Errorf("watchdog incident identity: %w", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("incident scope: %w", domain.ErrValidation)
	}
	switch incident.Status {
	case domain.AlertStatusFiring, domain.AlertStatusAcked, domain.AlertStatusResolved:
	default:
		return fmt.Errorf("incident status: %w", domain.ErrValidation)
	}
	if incident.TransitionVersion < 1 || incident.ConfigRevision < 1 || incident.StartedAt.IsZero() || len(incident.Reason) > 4096 {
		return fmt.Errorf("incident version or time: %w", domain.ErrValidation)
	}
	switch incident.SubjectKind {
	case domain.IncidentSubjectAvailability:
		if incident.ConditionKind != "" || incident.CertificateThreshold != 0 {
			return fmt.Errorf("availability subject: %w", domain.ErrValidation)
		}
	case domain.IncidentSubjectCapacity:
		if incident.ConditionKind != "session_pool" && incident.ConditionKind != "storage" {
			return fmt.Errorf("capacity subject: %w", domain.ErrValidation)
		}
	case domain.IncidentSubjectCertificate:
		if incident.CertificateThreshold != 30 && incident.CertificateThreshold != 14 && incident.CertificateThreshold != 7 {
			return fmt.Errorf("certificate subject: %w", domain.ErrValidation)
		}
	case domain.IncidentSubjectWatchdog:
	default:
		return fmt.Errorf("incident subject: %w", domain.ErrValidation)
	}
	if incident.Status == domain.AlertStatusFiring && (incident.ResolvedAt != nil || incident.AckedAt != nil) {
		return fmt.Errorf("firing incident times: %w", domain.ErrValidation)
	}
	if incident.Status == domain.AlertStatusAcked && (incident.ResolvedAt != nil || incident.AckedAt == nil) {
		return fmt.Errorf("acked incident times: %w", domain.ErrValidation)
	}
	if incident.Status == domain.AlertStatusResolved && incident.ResolvedAt == nil {
		return fmt.Errorf("resolved incident times: %w", domain.ErrValidation)
	}
	incident.StartedAt = incident.StartedAt.UTC()
	if incident.ResolvedAt != nil {
		t := incident.ResolvedAt.UTC()
		incident.ResolvedAt = &t
	}
	if incident.AckedAt != nil {
		t := incident.AckedAt.UTC()
		incident.AckedAt = &t
	}
	return nil
}

func validateDelivery(delivery *domain.RegionalDelivery) error {
	id, err := canonicalIdentity("delivery_id", delivery.DeliveryID)
	if err != nil {
		return err
	}
	delivery.DeliveryID = id
	alertID, err := canonicalIdentity("source_alert_id", delivery.SourceAlertID)
	if err != nil {
		return err
	}
	delivery.SourceAlertID = alertID
	switch delivery.EventKind {
	case domain.DeliveryEventStatusChange, domain.DeliveryEventCertificateExpiry, domain.DeliveryEventCapacityCondition,
		domain.DeliveryEventProbeConnection, domain.DeliveryEventIncidentSummary:
	default:
		return fmt.Errorf("delivery event kind: %w", domain.ErrValidation)
	}
	switch delivery.Status {
	case domain.DeliveryStatusSent, domain.DeliveryStatusSuperseded:
		if delivery.ErrorCode != "" {
			return fmt.Errorf("delivery error: %w", domain.ErrValidation)
		}
	case domain.DeliveryStatusRetrying, domain.DeliveryStatusFailed:
		if delivery.ErrorCode == "" {
			return fmt.Errorf("delivery error: %w", domain.ErrValidation)
		}
	default:
		return fmt.Errorf("delivery status: %w", domain.ErrValidation)
	}
	if delivery.Status == domain.DeliveryStatusSuperseded {
		if delivery.Attempt < 0 {
			return fmt.Errorf("delivery attempt: %w", domain.ErrValidation)
		}
	} else if delivery.Attempt < 1 {
		return fmt.Errorf("delivery attempt: %w", domain.ErrValidation)
	}
	if delivery.SourceTransitionVersion < 1 || delivery.NotificationID < 1 || delivery.NotificationVersion < 1 ||
		delivery.ProbeID == "" || delivery.ObservedAt.IsZero() {
		return fmt.Errorf("delivery identity: %w", domain.ErrValidation)
	}
	delivery.ObservedAt = delivery.ObservedAt.UTC()
	return nil
}

func canonicalIdentity(name, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 36 || value == "00000000-0000-0000-0000-000000000000" {
		return "", fmt.Errorf("%s: %w", name, domain.ErrValidation)
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character == '-' {
				continue
			}
		} else if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' {
			continue
		}
		return "", fmt.Errorf("%s: %w", name, domain.ErrValidation)
	}
	return value, nil
}

func incidentIdentityConflict(existing, incoming *probeIncidentModel) error {
	if existing.Scope != incoming.Scope || existing.ProbeID != incoming.ProbeID ||
		existing.SubjectKind != incoming.SubjectKind || !sameOptionalInt64(existing.MonitorID, incoming.MonitorID) ||
		!sameOptionalInt64(existing.AssignmentGeneration, incoming.AssignmentGeneration) ||
		!existing.StartedAt.UTC().Equal(incoming.StartedAt.UTC()) ||
		!sameOptionalString(existing.ConditionKind, incoming.ConditionKind) ||
		!sameOptionalInt(existing.CertificateThreshold, incoming.CertificateThreshold) {
		return ports.ErrConflict
	}
	return nil
}

func sameIncidentSnapshot(existing, incoming *probeIncidentModel) bool {
	return existing.Status == incoming.Status && existing.Reason == incoming.Reason &&
		existing.ConfigRevision == incoming.ConfigRevision &&
		sameOptionalTime(existing.ResolvedAt, incoming.ResolvedAt) &&
		sameOptionalTime(existing.AckedAt, incoming.AckedAt)
}

func incidentModel(in domain.RegionalIncident) probeIncidentModel {
	m := probeIncidentModel{
		HubIncidentID: in.HubIncidentID, SourceAlertID: in.SourceAlertID, Scope: string(in.Scope),
		ProbeID: in.ProbeID, Status: in.Status, TransitionVersion: in.TransitionVersion,
		StartedAt: in.StartedAt.UTC(), Reason: in.Reason, ConfigRevision: in.ConfigRevision,
		SubjectKind: in.SubjectKind,
	}
	if in.MonitorID > 0 {
		id := in.MonitorID
		m.MonitorID = &id
	}
	if in.AssignmentGeneration > 0 {
		g := in.AssignmentGeneration
		m.AssignmentGeneration = &g
	}
	m.ResolvedAt = utcPtr(in.ResolvedAt)
	m.AckedAt = utcPtr(in.AckedAt)
	if in.ConditionKind != "" {
		k := in.ConditionKind
		m.ConditionKind = &k
	}
	if in.CertificateThreshold > 0 {
		v := int(in.CertificateThreshold)
		m.CertificateThreshold = &v
	}
	if in.AckCommandID != "" {
		v := in.AckCommandID
		m.AckCommandID = &v
	}
	if in.AckActorDisplayName != "" {
		v := in.AckActorDisplayName
		m.AckActorDisplayName = &v
	}
	m.AckNote = in.AckNote
	if in.EscalationPolicyID > 0 {
		v := in.EscalationPolicyID
		m.EscalationPolicyID = &v
	}
	if in.EscalationPolicyVersion > 0 {
		v := in.EscalationPolicyVersion
		m.EscalationPolicyVersion = &v
	}
	if in.EscalationStatus != "" {
		v := in.EscalationStatus
		m.EscalationStatus = &v
	}
	m.EscalationNextStep = in.EscalationNextStep
	m.EscalationNextRunAt = utcPtr(in.EscalationNextRunAt)
	return m
}

func incidentFromModel(m *probeIncidentModel) domain.RegionalIncident {
	out := domain.RegionalIncident{
		HubIncidentID: m.HubIncidentID, SourceAlertID: m.SourceAlertID,
		Scope: domain.IncidentScope(m.Scope), ProbeID: m.ProbeID, Status: m.Status,
		TransitionVersion: m.TransitionVersion, StartedAt: m.StartedAt.UTC(),
		Reason: m.Reason, ConfigRevision: m.ConfigRevision, SubjectKind: m.SubjectKind,
		AckNote: m.AckNote, EscalationNextStep: m.EscalationNextStep,
	}
	if m.MonitorID != nil {
		out.MonitorID = *m.MonitorID
	}
	if m.AssignmentGeneration != nil {
		out.AssignmentGeneration = *m.AssignmentGeneration
	}
	out.ResolvedAt = utcPtr(m.ResolvedAt)
	out.AckedAt = utcPtr(m.AckedAt)
	if m.ConditionKind != nil {
		out.ConditionKind = *m.ConditionKind
	}
	if m.CertificateThreshold != nil {
		out.CertificateThreshold = int64(*m.CertificateThreshold)
	}
	if m.AckCommandID != nil {
		out.AckCommandID = *m.AckCommandID
	}
	if m.AckActorDisplayName != nil {
		out.AckActorDisplayName = *m.AckActorDisplayName
	}
	if m.EscalationPolicyID != nil {
		out.EscalationPolicyID = *m.EscalationPolicyID
	}
	if m.EscalationPolicyVersion != nil {
		out.EscalationPolicyVersion = *m.EscalationPolicyVersion
	}
	if m.EscalationStatus != nil {
		out.EscalationStatus = *m.EscalationStatus
	}
	out.EscalationNextRunAt = utcPtr(m.EscalationNextRunAt)
	return out
}

func deliveryModel(in domain.RegionalDelivery) probeDeliveryModel {
	m := probeDeliveryModel{
		DeliveryID: in.DeliveryID, SourceAlertID: in.SourceAlertID,
		SourceTransitionVersion: in.SourceTransitionVersion, ProbeID: in.ProbeID,
		NotificationID: in.NotificationID, NotificationVersion: in.NotificationVersion,
		EventKind: in.EventKind, Attempt: in.Attempt, Status: in.Status, ObservedAt: in.ObservedAt.UTC(),
	}
	if in.ErrorCode != "" {
		v := in.ErrorCode
		m.ErrorCode = &v
	}
	return m
}

func deliveryFromModel(m *probeDeliveryModel) domain.RegionalDelivery {
	out := domain.RegionalDelivery{
		DeliveryID: m.DeliveryID, SourceAlertID: m.SourceAlertID,
		SourceTransitionVersion: m.SourceTransitionVersion, ProbeID: m.ProbeID,
		NotificationID: m.NotificationID, NotificationVersion: m.NotificationVersion,
		EventKind: m.EventKind, Attempt: m.Attempt, Status: m.Status, ObservedAt: m.ObservedAt.UTC(),
	}
	if m.ErrorCode != nil {
		out.ErrorCode = *m.ErrorCode
	}
	return out
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func sameOptionalInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.UTC().Equal(b.UTC())
}
