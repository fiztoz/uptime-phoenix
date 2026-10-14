package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Genuine hub aggregate work. These incidents, intents and deliveries are
// separate from aggregate_standby_* planning and never promoted from held
// standby plans. Admission requires an active complete application barrier
// plus independently fenced ownership and current captured evidence.

type operativeAggregateIncidentModel struct {
	bun.BaseModel      `bun:"table:operative_aggregate_incidents"`
	IncidentID         string `bun:"incident_id,pk"`
	HubID              string
	MonitorID          int64
	BarrierID          string
	AssignmentRevision int64
	HealthPolicy       domain.HealthPolicy
	AlertDelivery      domain.AlertDelivery
	TransitionVersion  int64
	Status             string
	OpenedAt           time.Time
	ClosedAt           *time.Time
	CloseReason        string
}

type operativeAggregateIntentModel struct {
	bun.BaseModel        `bun:"table:operative_aggregate_intents"`
	IntentID             string `bun:"intent_id,pk"`
	IncidentID           string
	MonitorID            int64
	BarrierID            string
	TransitionVersion    int64
	NotificationID       int64
	SettingsSHA256       string `bun:"settings_sha256"`
	IncludeTarget        bool
	NotificationSnapshot string `bun:"notification_snapshot"`
	TemplateSnapshot     string `bun:"template_snapshot,nullzero"`
	Kind                 string
	Status               string
	CreatedAt            time.Time
	FinalizedAt          *time.Time
	Reason               string
}

type operativeAggregateDeliveryModel struct {
	bun.BaseModel     `bun:"table:operative_aggregate_deliveries"`
	DeliveryID        string `bun:"delivery_id,pk"`
	IntentID          string
	IncidentID        string
	MonitorID         int64
	BarrierID         string
	PrimaryProbeID    string
	NotificationID    int64
	TransitionVersion int64
	EventKind         string
	AvailableAt       time.Time
	Status            string
	Attempt           int64
	LeaseOwner        string
	LeaseEpoch        int64
	LeaseToken        string
	LeasedAt          *time.Time
	LeaseUntil        *time.Time
	ErrorCode         string
	OutcomeAt         *time.Time
	CreatedAt         time.Time
}

// operativeNotificationSnapshot is the captured provider settings of one
// aggregate channel. It is internal grant material: never an HTTP view, never
// logged. Identity excludes timestamps; the settings hash covers identity only.
type operativeNotificationSnapshot struct {
	ID            int64          `json:"id"`
	UserID        int64          `json:"user_id"`
	Name          string         `json:"name"`
	Type          string         `json:"type"`
	Active        bool           `json:"active"`
	IsDefault     bool           `json:"is_default"`
	IncludeAckURL bool           `json:"include_ack_url"`
	TemplateID    *int64         `json:"template_id"`
	Config        map[string]any `json:"config"`
}

type operativeTemplateSnapshot struct {
	ID            int64          `json:"id"`
	UserID        int64          `json:"user_id"`
	Name          string         `json:"name"`
	Provider      string         `json:"provider"`
	TitleTemplate string         `json:"title_template"`
	BodyTemplate  string         `json:"body_template"`
	Config        map[string]any `json:"config"`
}

// operativeChannelCapture is one channel's captured settings and its exact
// identity. Private settings are consumed only to derive identity and to be
// returned through the protected grant; they are never rendered here.
type operativeChannelCapture struct {
	NotificationID int64
	SettingsSHA256 string
	IncludeTarget  bool
	IncludeAckURL  bool
	Template       *operativeTemplateSnapshot
	Notification   operativeNotificationSnapshot
}

// operativeChannelIdentity builds the canonical data-only identity input. It
// deliberately excludes Bun metadata and storage clocks so timestamp precision
// is never identity.
func operativeChannelIdentity(notification NotificationModel, includeTarget bool, template *NotificationTemplateModel) (string, error) {
	snapshot := struct {
		Type        string
		Config      JSONField
		Ack, Target bool
		Template    *domain.NotificationTemplate
	}{Type: notification.Type, Config: notification.Config, Ack: notification.IncludeAckURL, Target: includeTarget}
	if template != nil {
		snapshot.Template = template.ToDomain()
		snapshot.Template.CreatedAt = time.Time{}
		snapshot.Template.UpdatedAt = time.Time{}
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	clear(b)
	return fmt.Sprintf("%x", sum), nil
}

// readOperativeChannelsTx captures every current active channel of the monitor,
// including include_target and template, with its exact identity.
func readOperativeChannelsTx(ctx context.Context, tx bun.Tx, monitorID int64) ([]operativeChannelCapture, error) {
	var rows []struct {
		NotificationModel `bun:",extend"`
		IncludeTarget     bool
	}
	if err := tx.NewSelect().Model(&rows).ModelTableExpr("notifications AS notification_model").
		ColumnExpr("notification_model.*,mn.include_target").Join("JOIN monitor_notification mn ON mn.notification_id=notification_model.id").
		Where("mn.monitor_id=? AND notification_model.active=?", monitorID, true).OrderExpr("notification_model.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]operativeChannelCapture, 0, len(rows))
	for i := range rows {
		n := rows[i]
		var template *NotificationTemplateModel
		if n.TemplateID != nil {
			template = new(NotificationTemplateModel)
			if err := tx.NewSelect().Model(template).Where("id=?", *n.TemplateID).Scan(ctx); err != nil {
				return nil, err
			}
		}
		identity, err := operativeChannelIdentity(n.NotificationModel, rows[i].IncludeTarget, template)
		if err != nil {
			return nil, err
		}
		capture := operativeChannelCapture{NotificationID: n.ID, SettingsSHA256: identity, IncludeTarget: rows[i].IncludeTarget,
			IncludeAckURL: n.IncludeAckURL}
		capture.Notification = operativeNotificationSnapshot{ID: n.ID, UserID: n.UserID, Name: n.Name, Type: n.Type,
			Active: n.Active, IsDefault: n.IsDefault, IncludeAckURL: n.IncludeAckURL, TemplateID: n.TemplateID}
		for k, v := range n.Config.ToMap() {
			if capture.Notification.Config == nil {
				capture.Notification.Config = map[string]any{}
			}
			capture.Notification.Config[k] = v
		}
		if template != nil {
			capture.Template = &operativeTemplateSnapshot{ID: template.ID, UserID: template.UserID, Name: template.Name,
				Provider: template.Provider, TitleTemplate: template.TitleTemplate, BodyTemplate: template.BodyTemplate,
				Config: template.Config.ToMap()}
		}
		out = append(out, capture)
	}
	return out, nil
}

// operativeChannelDigestSnapshot is the only channel material persisted with a
// queued aggregate plan: nonsecret identity plus the settings digest. Plaintext
// provider configuration (credentials included) never enters the queue; final
// authorization re-reads the current channel and must prove this exact digest
// before any settings are granted.
type operativeChannelDigestSnapshot struct {
	NotificationID int64  `json:"id"`
	Type           string `json:"type"`
	SettingsSHA256 string `json:"settings_sha256"`
}

// encodeDigest renders the credential-free identity of one captured channel.
// The digest covers the full settings identity (type, config, ack/target flags
// and template); the plaintext itself is never persisted with a plan.
func (c operativeChannelCapture) encodeDigest() (string, error) {
	b, err := json.Marshal(operativeChannelDigestSnapshot{NotificationID: c.NotificationID, Type: c.Notification.Type, SettingsSHA256: c.SettingsSHA256})
	if err != nil {
		return "", err
	}
	defer clear(b)
	return string(b), nil
}

// operativeQueuedDeliveryTx reads one aggregate queue row through the existing
// outbox mapping (deliveryIntentModel.queued), so the returned claim fence and
// snapshot fields keep the established QueuedDelivery shape. Aggregate hub work
// carries no per-source snapshot identity: the immutable barrier is identity.
func operativeQueuedDeliveryTx(ctx context.Context, tx bun.Tx, deliveryID string) (domain.QueuedDelivery, error) {
	var row deliveryIntentModel
	err := tx.NewSelect().TableExpr("operative_aggregate_deliveries").ColumnExpr(`
 delivery_id, incident_id AS source_alert_id, transition_version AS source_transition_version,
 primary_probe_id AS probe_id, notification_id, 0 AS notification_version, event_kind,
 available_at, 0 AS escalation_policy_id, 0 AS escalation_step, monitor_id,
 0 AS assignment_generation, '' AS stream_id, 0 AS source_seq, 0 AS config_revision,
 0 AS check_status, '' AS check_output, available_at AS observed_at, '' AS incident_status,
 created_at AS started_at, NULL AS resolved_at, available_at, status, attempt,
 lease_token, leased_at, lease_until, error_code, outcome_at, created_at`).
		Where("delivery_id = ?", deliveryID).Scan(ctx, &row)
	if err != nil {
		return domain.QueuedDelivery{}, err
	}
	queued := row.queued()
	var kind string
	if err := tx.NewSelect().Table("operative_aggregate_intents").Column("kind").Where("intent_id = (SELECT intent_id FROM operative_aggregate_deliveries WHERE delivery_id = ?)", deliveryID).Scan(ctx, &kind); err != nil {
		return domain.QueuedDelivery{}, err
	}
	if kind == "recovery" {
		queued.CheckStatus = domain.StatusUp
	}
	return queued, nil
}

// operativeAggregatePrimaryProbe returns the deterministic primary source of
// one barrier: its first immutable member. Aggregate work is monitor-scoped;
// the primary source only anchors identity.
func operativeAggregatePrimaryProbe(members []operativeMemberModel) (operativeMemberModel, error) {
	if len(members) == 0 {
		return operativeMemberModel{}, ports.ErrConflict
	}
	return members[0], nil // Members are stored ordered by probe_id.
}

// aggregatePagingTx admits genuine aggregate incidents and intents only from an
// active complete application barrier under the stored claim. The pure
// ANY/ALL/UNKNOWN evaluator is reused for the health proposal; it confers no
// authority, and evidence from before the barrier activation boundary can never
// establish a transition.
func (r *OperativeDeliveryStore) aggregatePagingTx(ctx context.Context, tx bun.Tx, hub string, barrier operativeBarrierModel, members []operativeMemberModel, state *operativeStateModel, evaluator ports.AggregateCutoverEvaluator, now time.Time) (bool, error) {
	if barrier.ActivatedAt == nil {
		return false, ports.ErrConflict
	}
	in, resend, err := readStandbyInput(ctx, tx, state.MonitorID)
	if err != nil {
		return false, err
	}
	in.Now = now
	in.SnapshotAt = now
	in.Current = true
	in.PolicyEffectiveAt = barrier.ActivatedAt.UTC()
	// Standby planner state never gates genuine operative admission: only the
	// operative incident row below may hold an open transition, and a standby
	// open must never prevent a new operative open.
	in.OpenIncident = nil
	operativeAdoptEvidence(in.Evidence, barrier.ActivatedAt.UTC(), now)
	var open operativeAggregateIncidentModel
	err = tx.NewSelect().Model(&open).Where("monitor_id = ? AND status = ?", state.MonitorID, "open").Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil {
		if open.BarrierID != barrier.BarrierID || open.TransitionVersion != state.TransitionVersion || open.OpenedAt.After(now) {
			return false, ports.ErrConflict
		}
		in.OpenIncident = &domain.AggregatePagingIncident{AssignmentRevision: open.AssignmentRevision, Policy: open.HealthPolicy, DeliveryMode: open.AlertDelivery}
	}
	decision, err := evaluator.EvaluateAggregate(in)
	if err != nil {
		return false, err
	}
	switch decision.Action {
	case domain.AggregatePagingOpen:
		if in.OpenIncident != nil || state.TransitionVersion == math.MaxInt64 {
			return false, ports.ErrConflict
		}
		id, err := uuid.NewRandom()
		if err != nil {
			return false, err
		}
		state.TransitionVersion++
		open = operativeAggregateIncidentModel{IncidentID: id.String(), HubID: hub, MonitorID: state.MonitorID,
			BarrierID: barrier.BarrierID, AssignmentRevision: in.AssignmentRevision, HealthPolicy: in.Policy,
			AlertDelivery: in.DeliveryMode, TransitionVersion: state.TransitionVersion, Status: "open", OpenedAt: now.UTC().Truncate(time.Microsecond)}
		if _, err := tx.NewInsert().Model(&open).Exec(ctx); err != nil {
			return false, err
		}
		if err := r.enqueueAggregateIntentsTx(ctx, tx, barrier, members, open, "down", now); err != nil {
			return false, err
		}
		return true, nil
	case domain.AggregatePagingRecover, domain.AggregatePagingAdminClose:
		if in.OpenIncident == nil || state.TransitionVersion == math.MaxInt64 {
			return false, ports.ErrConflict
		}
		if decision.Action == domain.AggregatePagingRecover && decision.Health.Status != domain.StatusUp {
			return false, ports.ErrConflict // UNKNOWN/PENDING health never recovers.
		}
		state.TransitionVersion++
		closed := now.UTC().Truncate(time.Microsecond)
		open.ClosedAt = &closed
		open.TransitionVersion = state.TransitionVersion
		reason := "recovered"
		kind := "recovery"
		if decision.Action == domain.AggregatePagingAdminClose {
			open.Status = "admin_closed"
			reason = decision.AdministrativeReason
			kind = ""
		} else {
			open.Status = "resolved"
		}
		open.CloseReason = reason
		if _, err := tx.NewUpdate().Model(&open).WherePK().Exec(ctx); err != nil {
			return false, err
		}
		superseded := "superseded"
		if decision.Action == domain.AggregatePagingAdminClose {
			superseded = "administrative"
		}
		if err := r.supersedeIncidentPlansTx(ctx, tx, open.IncidentID, superseded, now); err != nil {
			return false, err
		}
		if kind != "" {
			if err := r.enqueueAggregateIntentsTx(ctx, tx, barrier, members, open, kind, now); err != nil {
				return false, err
			}
		}
		return true, nil
	case domain.AggregatePagingHold:
		return r.admitAggregateRepeatTx(ctx, tx, barrier, members, state, open, decision, resend, now)
	default:
		return false, ports.ErrConflict
	}
}

// admitAggregateRepeatTx is the genuine repeat/resend admission for one open
// incident. A repeat is planned only for a still-fresh DOWN (UNKNOWN/PENDING
// neither repeats nor recovers), only after the durable last-planned admission
// plus the resend interval, and only while a channel exists to receive it. The
// stored intents are the durable last-planned record: no sent time is ever
// fabricated and no synthetic clock advances the throttle. A repeat supersedes
// obsolete unstarted work and replans it; started attempts keep their immutable
// history and resolve through their own fence.
func (r *OperativeDeliveryStore) admitAggregateRepeatTx(ctx context.Context, tx bun.Tx, barrier operativeBarrierModel, members []operativeMemberModel, state *operativeStateModel, open operativeAggregateIncidentModel, decision domain.AggregatePagingDecision, resend time.Duration, now time.Time) (bool, error) {
	if state.BarrierID == nil || open.IncidentID == "" {
		return false, nil // No operative open incident, no repeat.
	}
	if resend <= 0 || decision.Health.Status != domain.StatusDown {
		return false, nil
	}
	channels, err := readOperativeChannelsTx(ctx, tx, state.MonitorID)
	if err != nil {
		return false, err
	}
	if len(channels) == 0 {
		return false, nil // Nothing to resend; the throttle must not advance.
	}
	lastPlanned, err := operativeLastPlannedTx(ctx, tx, open.IncidentID, open.OpenedAt)
	if err != nil {
		return false, err
	}
	if now.Before(lastPlanned.Add(resend)) {
		return false, nil // Durable throttle: repeats wait for the interval.
	}
	if state.TransitionVersion == math.MaxInt64 {
		return false, ports.ErrConflict
	}
	state.TransitionVersion++
	open.TransitionVersion = state.TransitionVersion
	if _, err := tx.NewUpdate().Model(&open).WherePK().Exec(ctx); err != nil {
		return false, err
	}
	if err := r.supersedeIncidentPlansTx(ctx, tx, open.IncidentID, "superseded", now); err != nil {
		return false, err
	}
	if err := r.enqueueAggregateIntentsTx(ctx, tx, barrier, members, open, "repeat", now); err != nil {
		return false, err
	}
	return true, nil
}

// operativeLastPlannedTx returns the durable planning time of the incident's
// latest down/repeat admission: the queued intents themselves are the durable
// last-planned record. An incident admitted while no channel existed falls back
// to its opening time.
func operativeLastPlannedTx(ctx context.Context, tx bun.Tx, incidentID string, fallback time.Time) (time.Time, error) {
	var row struct {
		CreatedAt time.Time
	}
	err := tx.NewSelect().Table("operative_aggregate_intents").Column("created_at").
		Where("incident_id = ? AND kind IN (?, ?)", incidentID, "down", "repeat").
		OrderExpr("created_at DESC").Limit(1).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return fallback, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return row.CreatedAt.UTC(), nil
}

// operativeAdoptEvidence conservatively admits only evidence whose observation
// AND receipt both landed after this barrier activated, with no future receipt.
// Pre-activation standby-era samples and skewed future receipts can never open,
// recover or repeat an operative incident; an unknown region is never counted
// as fresh DOWN or as recovery.
func operativeAdoptEvidence(evidence []domain.RegionalHealthEvidence, activatedAt, now time.Time) {
	boundary := activatedAt.UTC()
	for i := range evidence {
		if evidence[i].UnknownReason != "" {
			continue
		}
		switch {
		case evidence[i].ReceivedAt.After(now):
			evidence[i].UnknownReason = "future_receipt"
		case !evidence[i].ObservedAt.After(boundary) || !evidence[i].ReceivedAt.After(boundary):
			evidence[i].UnknownReason = "before_operative_adoption"
		}
	}
}

// enqueueAggregateIntentsTx captures current channel identity/settings once per
// channel and records the immutable intent plus its queued aggregate delivery.
// Only the credential-free identity digest is persisted; plaintext provider
// settings are read back at final authorization under that digest proof.
func (r *OperativeDeliveryStore) enqueueAggregateIntentsTx(ctx context.Context, tx bun.Tx, barrier operativeBarrierModel, members []operativeMemberModel, incident operativeAggregateIncidentModel, kind string, now time.Time) error {
	if kind != "down" && kind != "recovery" && kind != "repeat" {
		return domain.ErrValidation
	}
	channels, err := readOperativeChannelsTx(ctx, tx, incident.MonitorID)
	if err != nil {
		return err
	}
	primary, err := operativeAggregatePrimaryProbe(members)
	if err != nil {
		return err
	}
	for _, channel := range channels {
		intentID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		digestJSON, err := channel.encodeDigest()
		if err != nil {
			return err
		}
		at := now.UTC().Truncate(time.Microsecond)
		intent := operativeAggregateIntentModel{IntentID: intentID.String(), IncidentID: incident.IncidentID, MonitorID: incident.MonitorID,
			BarrierID: barrier.BarrierID, TransitionVersion: incident.TransitionVersion, NotificationID: channel.NotificationID,
			SettingsSHA256: channel.SettingsSHA256, IncludeTarget: channel.IncludeTarget, Kind: kind, Status: "pending", CreatedAt: at,
			NotificationSnapshot: digestJSON}
		if _, err := tx.NewInsert().Model(&intent).Exec(ctx); err != nil {
			return err
		}
		deliveryID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		delivery := operativeAggregateDeliveryModel{DeliveryID: deliveryID.String(), IntentID: intent.IntentID, IncidentID: incident.IncidentID,
			MonitorID: incident.MonitorID, BarrierID: barrier.BarrierID, PrimaryProbeID: primary.ProbeID,
			NotificationID: channel.NotificationID, TransitionVersion: incident.TransitionVersion,
			EventKind: domain.DeliveryEventStatusChange, AvailableAt: at, Status: domain.DeliveryStatusPending, CreatedAt: at}
		if _, err := tx.NewInsert().Model(&delivery).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// operativeDeliveryStartedTx reports whether provider work was ever admitted
// for one delivery. Started work is never superseded into a fabricated outcome.
func operativeDeliveryStartedTx(ctx context.Context, tx bun.Tx, deliveryID string) (bool, error) {
	count, err := tx.NewSelect().Table("operative_provider_attempts").ColumnExpr("COUNT(*)").
		Where("delivery_id = ?", deliveryID).Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// supersedeIncidentPlansTx supersedes only unstarted work for one incident: a
// plan with an admitted provider grant is started work and is never rewritten
// here. Sent history is immutable.
func (r *OperativeDeliveryStore) supersedeIncidentPlansTx(ctx context.Context, tx bun.Tx, incidentID, reason string, now time.Time) error {
	if reason == "" {
		return domain.ErrValidation
	}
	at := now.UTC().Truncate(time.Microsecond)
	var deliveries []operativeAggregateDeliveryModel
	if err := tx.NewSelect().Model(&deliveries).Where("incident_id = ?", incidentID).Scan(ctx); err != nil {
		return err
	}
	for i := range deliveries {
		row := &deliveries[i]
		if row.Status != domain.DeliveryStatusPending && row.Status != domain.DeliveryStatusRetrying && row.Status != domain.DeliveryStatusLeased {
			continue
		}
		started, err := operativeDeliveryStartedTx(ctx, tx, row.DeliveryID)
		if err != nil {
			return err
		}
		if started {
			continue
		}
		if _, err := tx.NewUpdate().Model(row).Set("status = ?", domain.DeliveryStatusSuperseded).Set("outcome_at = ?", at).
			Set("lease_until = NULL").Set("leased_at = leased_at").WherePK().Exec(ctx); err != nil {
			return err
		}
	}
	_, err := tx.NewUpdate().Model((*operativeAggregateIntentModel)(nil)).Set("status = ?", "superseded").
		Set("reason = ?", reason).Set("finalized_at = ?", at).
		Where("incident_id = ? AND status = ?", incidentID, "pending").Exec(ctx)
	return err
}

// supersedeAggregatePlansTx stops new aggregate admissions and supersedes the
// monitor's unstarted plans during drain. Open genuine incidents close as
// administratively drained; no recovery or repeat is admitted from a drain.
func (r *OperativeDeliveryStore) supersedeAggregatePlansTx(ctx context.Context, tx bun.Tx, monitorID int64, barrierID string, now time.Time) (bool, error) {
	var open []operativeAggregateIncidentModel
	if err := tx.NewSelect().Model(&open).Where("monitor_id = ? AND barrier_id = ? AND status = ?", monitorID, barrierID, "open").Scan(ctx); err != nil {
		return false, err
	}
	changed := false
	at := now.UTC().Truncate(time.Microsecond)
	for i := range open {
		incident := &open[i]
		incident.Status = "admin_closed"
		incident.CloseReason = "drained"
		incident.ClosedAt = &at
		if _, err := tx.NewUpdate().Model(incident).WherePK().Exec(ctx); err != nil {
			return false, err
		}
		if err := r.supersedeIncidentPlansTx(ctx, tx, incident.IncidentID, "drained", now); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}
