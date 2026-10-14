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
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type standbyStateModel struct {
	bun.BaseModel      `bun:"table:aggregate_standby_state,alias:standby"`
	MonitorID          int64 `bun:"monitor_id,pk"`
	HubID              string
	OwnerID            string
	LeaseEpoch         int64
	LeaseUntil         time.Time
	DueAt              time.Time
	EvaluatedAt        *time.Time
	TransitionVersion  int64
	NextRepeatAt       *time.Time
	LastPlannedAt      *time.Time
	AssignmentRevision int64
	PolicyEffectiveAt  *time.Time
}
type standbyIncidentModel struct {
	bun.BaseModel      `bun:"table:aggregate_standby_incidents,alias:incident"`
	IncidentID         string `bun:"incident_id,pk"`
	HubID              string
	MonitorID          int64
	OpenMonitorID      *int64
	AssignmentRevision int64
	HealthPolicy       domain.HealthPolicy
	AlertDelivery      domain.AlertDelivery
	TransitionVersion  int64
	Status             string
	OpenedAt           time.Time
	ClosedAt           *time.Time
	CloseReason        string
}
type standbyIntentModel struct {
	bun.BaseModel     `bun:"table:aggregate_standby_intents,alias:plan"`
	IntentID          string `bun:"intent_id,pk"`
	IncidentID        string
	TransitionVersion int64
	NotificationID    int64
	ChannelSHA256     string `bun:"channel_sha256"`
	Kind              string
	Status            string
	CreatedAt         time.Time
	InvalidatedAt     *time.Time
	Reason            string
}

// AggregateStandbyStore persists hub-owned planning; it implements no provider API.
type AggregateStandbyStore struct{ db *bun.DB }

// NewAggregateStandbyStore constructs a dialect-neutral standby-only adapter.
func NewAggregateStandbyStore(db *bun.DB) *AggregateStandbyStore {
	return &AggregateStandbyStore{db: db}
}

var _ ports.AggregateStandbyRepository = (*AggregateStandbyStore)(nil)

// ClaimStandby claims due desired aggregate/both monitors or obsolete open plans.
// Each expired acquisition advances the epoch, even for the same process owner.
func (r *AggregateStandbyStore) ClaimStandby(ctx context.Context, owner string, ttl time.Duration, limit int) ([]domain.AggregateStandbyLease, error) {
	if r == nil || r.db == nil || !validStandbyUUID(owner) || ttl < time.Second || ttl > time.Minute || limit < 1 || limit > 100 {
		return nil, domain.ErrValidation
	}
	// The first transaction discovers selectors only, never authority. Each
	// monitor acquires independently so one corrupt/exhausted lease cannot roll
	// back healthy claims or strand a fleet batch.
	var ids []int64
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		return tx.NewSelect().TableExpr("monitor_probe_assignment_sets AS s").ColumnExpr("s.monitor_id").
			Join("LEFT JOIN aggregate_standby_state AS a ON a.monitor_id=s.monitor_id").
			Where("(s.alert_delivery IN (?,?) OR EXISTS (SELECT 1 FROM aggregate_standby_incidents AS i WHERE i.open_monitor_id=s.monitor_id))", domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth).
			Where("a.monitor_id IS NULL OR (a.lease_until <= ? AND a.due_at <= ?)", now, now).
			OrderExpr("a.due_at ASC,s.monitor_id ASC").Limit(int64(limit)).Scan(ctx, &ids)
	})
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	var claims []domain.AggregateStandbyLease
	var failed error
	for _, id := range ids {
		if ctx.Err() != nil {
			return claims, errors.Join(failed, ctx.Err())
		}
		var claim *domain.AggregateStandbyLease
		err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
			claim = nil
			if _, err := tx.ExecContext(ctx, "UPDATE monitors SET id=id WHERE id=?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE aggregate_standby_state SET lease_epoch=lease_epoch WHERE monitor_id=?", id); err != nil {
				return err
			}
			var a standbyStateModel
			err := tx.NewSelect().Model(&a).Where("monitor_id=?", id).Scan(ctx)
			exists := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Recheck current eligibility, not the earlier selector snapshot.
			var eligible int
			if err := tx.NewRaw("SELECT COUNT(*) FROM monitor_probe_assignment_sets s WHERE s.monitor_id=? AND (s.alert_delivery IN (?,?) OR EXISTS(SELECT 1 FROM aggregate_standby_incidents i WHERE i.open_monitor_id=s.monitor_id))", id, domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth).Scan(ctx, &eligible); err != nil {
				return err
			}
			if eligible != 1 {
				return nil
			}
			var hub string
			if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id=1").Scan(ctx, &hub); err != nil {
				return err
			}
			if !validStandbyUUID(hub) {
				return ports.ErrConflict
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if exists {
				if a.HubID != hub || a.LeaseEpoch <= 0 || a.LeaseEpoch == math.MaxInt64 || a.TransitionVersion < 0 {
					return ports.ErrConflict
				}
				if a.LeaseUntil.After(now) || a.DueAt.After(now) {
					return nil
				}
			} else {
				a = standbyStateModel{MonitorID: id, HubID: hub}
			}
			a.OwnerID = owner
			a.LeaseEpoch++
			a.LeaseUntil = now.Add(ttl)
			a.DueAt = now
			if exists {
				_, err = tx.NewUpdate().Model(&a).WherePK().Exec(ctx)
			} else {
				_, err = tx.NewInsert().Model(&a).Exec(ctx)
			}
			if err != nil {
				return err
			}
			claim = &domain.AggregateStandbyLease{MonitorID: id, OwnerID: owner, Epoch: a.LeaseEpoch, Until: a.LeaseUntil}
			return nil
		})
		if err != nil {
			failed = errors.Join(failed, remoteSyncError(ctx, err))
			continue
		}
		if claim != nil {
			claims = append(claims, *claim)
		}
	}
	return claims, failed
}

func validStandbyUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
func standbyFence(a standbyStateModel, f domain.AggregateStandbyLease, now time.Time) bool {
	return a.MonitorID == f.MonitorID && a.OwnerID == f.OwnerID && a.LeaseEpoch == f.Epoch && a.LeaseUntil.Equal(f.Until) && a.LeaseUntil.After(now)
}

// ReconcileStandby captures current stored evidence and commits standby effects
// under the independently stored lease. Historical replay never invokes it.
func (r *AggregateStandbyStore) ReconcileStandby(ctx context.Context, f domain.AggregateStandbyLease, evaluator ports.AggregateStandbyEvaluator) (domain.AggregateStandbyResult, error) {
	var result domain.AggregateStandbyResult
	if r == nil || r.db == nil || evaluator == nil || f.MonitorID <= 0 || !validStandbyUUID(f.OwnerID) || f.Epoch <= 0 || f.Until.IsZero() {
		return result, domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		result = domain.AggregateStandbyResult{}
		if _, err := tx.ExecContext(ctx, "UPDATE aggregate_standby_state SET lease_epoch=lease_epoch WHERE monitor_id=?", f.MonitorID); err != nil {
			return err
		}
		var a standbyStateModel
		if err := tx.NewSelect().Model(&a).Where("monitor_id=?", f.MonitorID).Scan(ctx); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !standbyFence(a, f, now) || a.TransitionVersion < 0 || a.EvaluatedAt != nil && now.Before(a.EvaluatedAt.UTC()) {
			return ports.ErrConflict
		}
		var hub string
		if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id=1").Scan(ctx, &hub); err != nil {
			return err
		}
		if !validStandbyUUID(hub) || a.HubID != hub {
			return ports.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision=revision WHERE monitor_id=?", f.MonitorID); err != nil {
			return err
		}
		in, resend, err := readStandbyInput(ctx, tx, f.MonitorID)
		if err != nil {
			return err
		}
		var incident standbyIncidentModel
		err = tx.NewSelect().Model(&incident).Where("open_monitor_id=?", f.MonitorID).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if !validStandbyUUID(incident.IncidentID) || incident.HubID != a.HubID || incident.MonitorID != f.MonitorID || incident.Status != "open" || incident.OpenMonitorID == nil || *incident.OpenMonitorID != f.MonitorID || incident.TransitionVersion != a.TransitionVersion || incident.OpenedAt.After(now) {
				return ports.ErrConflict
			}
			in.OpenIncident = &domain.AggregatePagingIncident{AssignmentRevision: incident.AssignmentRevision, Policy: incident.HealthPolicy, DeliveryMode: incident.AlertDelivery}
		}
		// Capture after preceding locks, not before a potentially long lock wait.
		now, err = replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !standbyFence(a, f, now) || a.EvaluatedAt != nil && now.Before(a.EvaluatedAt.UTC()) {
			return ports.ErrConflict
		}
		// Assignment history has an application-clock boundary. Standby adopts
		// a revision conservatively on the DB clock, never relabeling previously
		// observed evidence as post-change. This is not source mode application.
		if a.AssignmentRevision > in.AssignmentRevision {
			return ports.ErrConflict
		}
		if a.AssignmentRevision != in.AssignmentRevision {
			a.AssignmentRevision = in.AssignmentRevision
			a.PolicyEffectiveAt = &now
		}
		if a.PolicyEffectiveAt == nil {
			return ports.ErrConflict
		}
		in.PolicyEffectiveAt = a.PolicyEffectiveAt.UTC()
		for i := range in.Evidence {
			if !in.Evidence[i].ReceivedAt.After(in.PolicyEffectiveAt) || in.Evidence[i].ReceivedAt.After(now) {
				in.Evidence[i].UnknownReason = "before_standby_adoption"
			}
		}
		in.Now = now
		in.SnapshotAt = now
		in.Current = true
		decision, err := evaluator.EvaluateStandby(in)
		if err != nil {
			return err
		}
		channels, err := readStandbyChannels(ctx, tx, f.MonitorID)
		if err != nil {
			return err
		}
		if err := invalidateStandbyChannels(ctx, tx, f.MonitorID, channels, now); err != nil {
			return err
		}
		kind := ""
		switch decision.Action {
		case domain.AggregatePagingOpen:
			if in.OpenIncident != nil || a.TransitionVersion == math.MaxInt64 {
				return ports.ErrConflict
			}
			id, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			a.TransitionVersion++
			incident = standbyIncidentModel{IncidentID: id.String(), HubID: a.HubID, MonitorID: f.MonitorID, OpenMonitorID: &f.MonitorID, AssignmentRevision: in.AssignmentRevision, HealthPolicy: in.Policy, AlertDelivery: in.DeliveryMode, TransitionVersion: a.TransitionVersion, Status: "open", OpenedAt: now}
			if _, err := tx.NewInsert().Model(&incident).Exec(ctx); err != nil {
				return err
			}
			kind = "down"
		case domain.AggregatePagingRecover, domain.AggregatePagingAdminClose:
			if in.OpenIncident == nil || a.TransitionVersion == math.MaxInt64 {
				return ports.ErrConflict
			}
			a.TransitionVersion++
			incident.OpenMonitorID = nil
			incident.ClosedAt = &now
			incident.TransitionVersion = a.TransitionVersion
			incident.Status = "resolved"
			incident.CloseReason = "recovered"
			kind = "recovery"
			reason := "superseded"
			if decision.Action == domain.AggregatePagingAdminClose {
				incident.Status = "admin_closed"
				incident.CloseReason = decision.AdministrativeReason
				reason = "administrative"
				kind = ""
			}
			if err := invalidateStandbyIncident(ctx, tx, incident.IncidentID, reason, now); err != nil {
				return err
			}
			if _, err := tx.NewUpdate().Model(&incident).WherePK().Exec(ctx); err != nil {
				return err
			}
			a.NextRepeatAt = nil
			a.LastPlannedAt = nil
		case domain.AggregatePagingHold:
			if in.OpenIncident != nil {
				if a.LastPlannedAt == nil || a.LastPlannedAt.After(now) {
					return ports.ErrConflict
				}
				a.NextRepeatAt = nil
				if resend > 0 {
					next := a.LastPlannedAt.UTC().Add(resend)
					a.NextRepeatAt = &next
				}
			}
			if in.OpenIncident != nil && decision.Health.Status == domain.StatusDown && resend > 0 && a.NextRepeatAt != nil && !now.Before(a.NextRepeatAt.UTC()) {
				if a.TransitionVersion == math.MaxInt64 {
					return ports.ErrConflict
				}
				a.TransitionVersion++
				incident.TransitionVersion = a.TransitionVersion
				kind = "repeat"
				if err := invalidateStandbyIncident(ctx, tx, incident.IncidentID, "superseded", now); err != nil {
					return err
				}
				if _, err := tx.NewUpdate().Model(&incident).WherePK().Exec(ctx); err != nil {
					return err
				}
			}
		default:
			return ports.ErrConflict
		}
		if kind != "" {
			for _, channel := range channels {
				id, err := uuid.NewRandom()
				if err != nil {
					return err
				}
				row := standbyIntentModel{IntentID: id.String(), IncidentID: incident.IncidentID, TransitionVersion: a.TransitionVersion, NotificationID: channel.NotificationID, ChannelSHA256: channel.RevisionSHA256, Kind: kind, Status: "held", CreatedAt: now}
				if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
					return err
				}
			}
			if kind != "recovery" {
				a.LastPlannedAt = &now
				a.NextRepeatAt = nil
				if resend > 0 {
					next := now.Add(resend)
					a.NextRepeatAt = &next
				}
			}
		}
		after, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !standbyFence(a, f, after) || after.Before(now) {
			return ports.ErrConflict
		}
		a.EvaluatedAt = &now
		a.LeaseUntil = after
		a.DueAt = after.Add(5 * time.Second)
		if _, err := tx.NewUpdate().Model(&a).WherePK().Exec(ctx); err != nil {
			return err
		}
		result = domain.AggregateStandbyResult{MonitorID: f.MonitorID, IncidentID: incident.IncidentID, TransitionVersion: a.TransitionVersion, Action: decision.Action, Health: decision.Health}
		return nil
	})
	if err != nil {
		return domain.AggregateStandbyResult{}, remoteSyncError(ctx, err)
	}
	return result, nil
}

func readStandbyInput(ctx context.Context, tx bun.Tx, id int64) (domain.AggregatePagingInput, time.Duration, error) {
	var in domain.AggregatePagingInput
	var monitor MonitorModel
	if err := tx.NewSelect().Model(&monitor).Column("active", "check_interval", "retry_interval", "timeout", "resend_interval").Where("id=?", id).Scan(ctx); err != nil {
		return in, 0, err
	}
	members, err := readProbeAssignments(ctx, tx, id)
	if err != nil {
		return in, 0, err
	}
	fresh, err := services.RegionalFreshnessWindow(monitor.Interval, monitor.RetryInterval, monitor.Timeout)
	if err != nil {
		return in, 0, err
	}
	in.Policy = members.HealthPolicy
	in.DeliveryMode = members.AlertDelivery
	in.AssignmentRevision = members.Revision
	for _, m := range members.Assignments {
		e := domain.RegionalHealthEvidence{ProbeID: m.ProbeID, FreshFor: fresh, Paused: !monitor.Active, UnknownReason: "missing_evidence"}
		var state monitorProbeStateModel
		err := tx.NewSelect().Model(&state).Where("monitor_id=? AND probe_id=?", id, m.ProbeID).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return in, 0, err
		}
		if err == nil {
			e.Status = domain.Status(state.Status)
			e.ObservedAt = state.ObservedAt.UTC()
			e.ReceivedAt = state.ReceivedAt.UTC()
			e.UnknownReason = ""
			if state.AssignmentGeneration != m.Generation {
				e.UnknownReason = "stale_generation"
			} else if state.Seq < 1 {
				e.UnknownReason = "missing_evidence"
			} else if m.ProbeID == domain.LocalProbeID {
				if state.StreamID != domain.LocalStreamID {
					e.UnknownReason = "unaccepted_stream"
				}
			} else {
				var count int
				err = tx.NewRaw(`SELECT COUNT(*) FROM probes p JOIN probe_connections c ON c.probe_id=p.id
      JOIN probe_streams s ON s.probe_id=p.id AND s.stream_id=c.stream_id
      WHERE p.id=? AND p.enabled=? AND p.revoked_at IS NULL AND p.deleted_at IS NULL
      AND c.stream_id=? AND s.retired_at IS NULL
      AND NOT EXISTS(SELECT 1 FROM probe_stream_resets r WHERE r.probe_id=p.id AND r.state='prepared')`, m.ProbeID, true, state.StreamID).Scan(ctx, &count)
				if err != nil {
					return in, 0, err
				}
				if count != 1 {
					e.UnknownReason = "unaccepted_stream"
				}
			}
		}
		in.Evidence = append(in.Evidence, e)
	}
	if monitor.ResendInterval < 0 || int64(monitor.ResendInterval) > math.MaxInt64/int64(time.Minute) {
		return in, 0, ports.ErrConflict
	}
	return in, time.Duration(monitor.ResendInterval) * time.Minute, nil
}

func readStandbyChannels(ctx context.Context, tx bun.Tx, monitor int64) ([]domain.AggregateStandbyChannel, error) {
	// Private settings are consumed only to derive exact identity, never persisted,
	// returned in errors/views or rendered here. Timestamp precision is not identity.
	var rows []struct {
		NotificationModel `bun:",extend"`
		IncludeTarget     bool
	}
	if err := tx.NewSelect().Model(&rows).ModelTableExpr("notifications AS notification_model").
		ColumnExpr("notification_model.*,mn.include_target").Join("JOIN monitor_notification mn ON mn.notification_id=notification_model.id").
		Where("mn.monitor_id=? AND notification_model.active=?", monitor, true).OrderExpr("notification_model.id ASC").Scan(ctx); err != nil {
		return nil, err
	}
	out := make([]domain.AggregateStandbyChannel, 0, len(rows))
	for _, n := range rows {
		var template *NotificationTemplateModel
		if n.TemplateID != nil {
			template = new(NotificationTemplateModel)
			if err := tx.NewSelect().Model(template).Where("id=?", *n.TemplateID).Scan(ctx); err != nil {
				return nil, err
			}
		}
		// Explicit data-only canonical input excludes Bun metadata and storage clocks.
		snapshot := struct {
			Type        string
			Config      JSONField
			Ack, Target bool
			Template    *domain.NotificationTemplate
		}{Type: n.Type, Config: n.Config, Ack: n.IncludeAckURL, Target: n.IncludeTarget}
		if template != nil {
			snapshot.Template = template.ToDomain()
			snapshot.Template.CreatedAt = time.Time{}
			snapshot.Template.UpdatedAt = time.Time{}
		}
		b, err := json.Marshal(snapshot)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		clear(b)
		out = append(out, domain.AggregateStandbyChannel{NotificationID: n.ID, RevisionSHA256: fmt.Sprintf("%x", sum)})
	}
	return out, nil
}
func invalidateStandbyChannels(ctx context.Context, tx bun.Tx, monitor int64, channels []domain.AggregateStandbyChannel, now time.Time) error {
	var rows []standbyIntentModel
	if err := tx.NewSelect().Model(&rows).Join("JOIN aggregate_standby_incidents i ON i.incident_id=plan.incident_id").Where("i.monitor_id=? AND plan.status='held'", monitor).Scan(ctx); err != nil {
		return err
	}
	current := make(map[int64]string, len(channels))
	for _, c := range channels {
		current[c.NotificationID] = c.RevisionSHA256
	}
	for _, p := range rows {
		if current[p.NotificationID] != p.ChannelSHA256 {
			if now.Before(p.CreatedAt.UTC()) {
				return ports.ErrConflict
			}
			if _, err := tx.NewUpdate().Model((*standbyIntentModel)(nil)).Set("status='invalidated'").Set("reason='channel_changed'").Set("invalidated_at=?", now).Where("intent_id=? AND status='held'", p.IntentID).Exec(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func invalidateStandbyIncident(ctx context.Context, tx bun.Tx, id, reason string, now time.Time) error {
	_, err := tx.NewUpdate().Model((*standbyIntentModel)(nil)).Set("status='invalidated'").Set("reason=?", reason).Set("invalidated_at=?", now).Where("incident_id=? AND status='held'", id).Exec(ctx)
	return err
}
