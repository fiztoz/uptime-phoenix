package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ClaimAggregateDelivery reserves one due genuine aggregate delivery under an
// independent DB-clock lease. Expired leases are reclaimable with a new fence
// and incremented attempt. No provider I/O occurs here; the returned work comes
// from stored rows, never a caller snapshot.
func (r *OperativeDeliveryStore) ClaimAggregateDelivery(ctx context.Context, owner string, ttl time.Duration) (*domain.QueuedDelivery, error) {
	if r == nil || r.db == nil || !validStandbyUUID(owner) || ttl < time.Second || ttl > 15*time.Minute {
		return nil, domain.ErrValidation
	}
	var out *domain.QueuedDelivery
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		var rows []operativeAggregateDeliveryModel
		q := tx.NewSelect().Model(&rows).
			Where("(status IN (?, ?) AND available_at <= ?) OR (status = ? AND lease_until <= ?)",
				domain.DeliveryStatusPending, domain.DeliveryStatusRetrying, now, domain.DeliveryStatusLeased, now).
			Where("attempt < ?", int64(math.MaxInt64)).
			OrderExpr("available_at ASC, created_at ASC, delivery_id ASC").Limit(1)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE SKIP LOCKED")
		}
		if err := q.Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		row := rows[0]
		fence := rand.Text()
		until := now.Add(ttl)
		if _, err := tx.NewUpdate().Model((*operativeAggregateDeliveryModel)(nil)).
			Set("status = ?", domain.DeliveryStatusLeased).
			Set("attempt = attempt + 1").
			Set("lease_owner = ?", owner).
			Set("lease_epoch = lease_epoch + 1").
			Set("lease_token = ?", fence).
			Set("leased_at = ?", now.UTC().Truncate(time.Microsecond)).
			Set("lease_until = ?", until.UTC().Truncate(time.Microsecond)).
			Where("delivery_id = ? AND attempt = ?", row.DeliveryID, row.Attempt).Exec(ctx); err != nil {
			return err
		}
		queued, err := operativeQueuedDeliveryTx(ctx, tx, row.DeliveryID)
		if err != nil {
			return err
		}
		out = &queued
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	if out == nil {
		return nil, ports.ErrNotFound
	}
	return out, nil
}

// AuthorizeAggregateDelivery is the final DB transaction for one hub aggregate
// provider attempt. It rechecks the stored owner/epoch/deadline of the claim,
// that the lease covers the whole provider budget, the active complete
// application barrier with all-source current set/config/application and
// generation/session/capability evidence, and the captured channel identity/
// settings digest/include-target, then journals one opaque attempt grant in the
// same transaction. The returned grant settings come from the single captured
// current-channel read proven identical to the plan by the digest; they are
// never an HTTP view and the plan queue never carries plaintext settings.
func (r *OperativeDeliveryStore) AuthorizeAggregateDelivery(ctx context.Context, claim domain.DeliveryClaim, budget time.Duration) (*domain.AggregateProviderGrant, error) {
	if r == nil || r.db == nil || claim.Attempt < 1 || claim.LeaseToken == "" || len(claim.LeaseToken) > 64 ||
		!domain.ValidHubID(claim.DeliveryID) || (claim.ProbeID != domain.LocalProbeID && !validRemoteProbeID(claim.ProbeID)) ||
		budget <= 0 || budget > operativeMaxProviderBudget {
		return nil, domain.ErrValidation
	}
	var grant *domain.AggregateProviderGrant
	var declined bool
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		grant = nil
		var row operativeAggregateDeliveryModel
		if err := tx.NewSelect().Model(&row).Where("delivery_id = ?", claim.DeliveryID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		// Stored owner/epoch/deadline: exact claim identity and one provider budget.
		if row.PrimaryProbeID != claim.ProbeID || row.Attempt != claim.Attempt || row.LeaseToken != claim.LeaseToken ||
			row.Status != domain.DeliveryStatusLeased || row.LeaseOwner == "" || row.LeaseEpoch <= 0 ||
			row.LeasedAt == nil || row.LeaseUntil == nil || now.Before(row.LeasedAt.UTC()) || !now.Add(budget).Before(row.LeaseUntil.UTC()) {
			return ports.ErrConflict
		}
		var intent operativeAggregateIntentModel
		if err := tx.NewSelect().Model(&intent).Where("intent_id = ?", row.IntentID).Scan(ctx); err != nil {
			return ports.ErrConflict
		}
		if intent.IncidentID != row.IncidentID || intent.MonitorID != row.MonitorID || intent.BarrierID != row.BarrierID ||
			intent.NotificationID != row.NotificationID || intent.TransitionVersion != row.TransitionVersion || intent.Status != "pending" {
			return ports.ErrConflict
		}
		var incident operativeAggregateIncidentModel
		if err := tx.NewSelect().Model(&incident).Where("incident_id = ?", row.IncidentID).Scan(ctx); err != nil {
			return ports.ErrConflict
		}
		if incident.MonitorID != row.MonitorID || incident.BarrierID != row.BarrierID || incident.TransitionVersion != row.TransitionVersion {
			return ports.ErrConflict
		}
		if (intent.Kind == "recovery" && (incident.Status != "resolved" || incident.CloseReason != "recovered")) ||
			(intent.Kind != "recovery" && incident.Status != "open") {
			return ports.ErrConflict
		}
		// Active complete application barrier with all-source current
		// set/config/application evidence.
		var state operativeStateModel
		if err := tx.NewSelect().Model(&state).Where("monitor_id = ?", row.MonitorID).Scan(ctx); err != nil {
			return ports.ErrConflict
		}
		if state.Phase != operativePhaseActive || state.BarrierID == nil || *state.BarrierID != row.BarrierID {
			return ports.ErrConflict
		}
		barrier, members, err := operativeBarrierTx(ctx, tx, row.BarrierID)
		if err != nil {
			return err
		}
		var hub string
		if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id=1").Scan(ctx, &hub); err != nil {
			return err
		}
		// Exact barrier identity and activation: the grant is bound to this
		// hub's activated barrier, never to a foreign, unactivated or renamed one.
		primary, err := operativeAggregatePrimaryProbe(members)
		if err != nil {
			return err
		}
		if !validStandbyUUID(hub) || state.HubID != hub || barrier.MonitorID != row.MonitorID || barrier.HubID != hub ||
			barrier.ActivatedAt == nil || barrier.AlertDelivery != state.AlertDelivery ||
			barrier.AssignmentRevision != state.AssignmentRevision || primary.ProbeID != row.PrimaryProbeID {
			return ports.ErrConflict
		}
		set, err := readProbeAssignments(ctx, tx, row.MonitorID)
		if err != nil {
			return err
		}
		// Invalidation of a changed channel is independently safe even while
		// its edit also makes the acknowledged whole-source graph stale. No
		// provider grant is emitted from this refusal path.
		// Captured channel identity/settings/template/include-target recheck.
		channels, err := readOperativeChannelsTx(ctx, tx, row.MonitorID)
		if err != nil {
			return err
		}
		var channel *operativeChannelCapture
		for i := range channels {
			if channels[i].NotificationID == row.NotificationID {
				channel = &channels[i]
				break
			}
		}
		if channel == nil || channel.SettingsSHA256 != intent.SettingsSHA256 || channel.IncludeTarget != intent.IncludeTarget {
			// Channel identity changed after the plan was admitted: the plan is
			// superseded as unstarted work and the attempt is never granted.
			if err := r.supersedeIncidentPlansTx(ctx, tx, incident.IncidentID, "channel_changed", now); err != nil {
				return err
			}
			finalNow, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if finalNow.Before(now) || !finalNow.Before(row.LeaseUntil.UTC()) {
				return ports.ErrConflict
			}
			declined = true
			return nil // Commit unstarted invalidation; report refusal afterward.
		}
		// The queued plan is digest-only: plaintext provider settings never
		// enter the queue. The grant below comes from this single captured
		// current channel read, proven identical to the plan by the digest.
		if intent.NotificationSnapshot != "" {
			var stored operativeChannelDigestSnapshot
			if err := json.Unmarshal([]byte(intent.NotificationSnapshot), &stored); err != nil {
				return ports.ErrConflict // Semantic failures are redacted, never repaired.
			}
			if stored.NotificationID != intent.NotificationID || stored.Type != channel.Notification.Type || stored.SettingsSHA256 != intent.SettingsSHA256 {
				return ports.ErrConflict
			}
		}
		drifted, corrupt, err := r.operativeDriftTx(ctx, tx, hub, barrier, members, &state, now)
		if err != nil {
			return err
		}
		if drifted || corrupt {
			return ports.ErrConflict
		}
		if len(set.Assignments) != len(members) {
			return ports.ErrConflict
		}
		for i := range members {
			ev, err := r.readOperativeMemberEvidenceTx(ctx, tx, hub, row.MonitorID, set.Assignments[i], barrier.AlertDelivery)
			if err != nil {
				return err
			}
			if ev.Evidence != operativeEvidenceComplete || !ev.Capable {
				return ports.ErrConflict
			}
			application, err := r.operativeApplicationTx(ctx, tx, hub, barrier.BarrierID, members[i], barrier.AlertDelivery)
			if err != nil {
				return err
			}
			if application == nil {
				return ports.ErrConflict
			}
		}
		var template *domain.NotificationTemplate
		if channel.Template != nil {
			template = &domain.NotificationTemplate{ID: channel.Template.ID, UserID: channel.Template.UserID, Name: channel.Template.Name,
				Provider: channel.Template.Provider, TitleTemplate: channel.Template.TitleTemplate, BodyTemplate: channel.Template.BodyTemplate,
				Config: channel.Template.Config}
		}
		var monitor MonitorModel
		if err := tx.NewSelect().Model(&monitor).Where("id = ?", row.MonitorID).Scan(ctx); err != nil {
			return err
		}
		regions, err := operativeRegionsTx(ctx, tx, members)
		if err != nil {
			return err
		}
		if err := r.requireOperativeSessionsTx(ctx, tx, hub, members, budget); err != nil {
			return err
		}
		finalNow, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if finalNow.Before(now) || !finalNow.Add(budget).Before(row.LeaseUntil.UTC()) {
			return ports.ErrConflict
		}
		now = finalNow
		attemptID, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		attempt := operativeAttemptModel{AttemptID: attemptID.String(), Scope: "aggregate", MonitorID: row.MonitorID,
			ProbeID: row.PrimaryProbeID, DeliveryID: row.DeliveryID, ClaimAttempt: row.Attempt,
			NotificationID: row.NotificationID, SettingsSHA256: intent.SettingsSHA256,
			BudgetUntil: now.Add(budget), State: string(domain.ProviderAttemptInFlight),
			CreatedAt: now.UTC().Truncate(time.Microsecond)}
		if !validStandbyUUID(attempt.AttemptID) {
			return ports.ErrConflict
		}
		// One provider budget per claim attempt.
		if _, err := tx.NewInsert().Model(&attempt).Exec(ctx); err != nil {
			return ports.ErrConflict
		}
		queued, err := operativeQueuedDeliveryTx(ctx, tx, row.DeliveryID)
		if err != nil {
			return err
		}
		out := domain.Notification{ID: channel.Notification.ID, UserID: channel.Notification.UserID, Name: channel.Notification.Name,
			Type: channel.Notification.Type, Active: channel.Notification.Active, IsDefault: channel.Notification.IsDefault,
			IncludeAckURL: channel.Notification.IncludeAckURL, TemplateID: channel.Notification.TemplateID, Config: channel.Notification.Config}
		grant = &domain.AggregateProviderGrant{AttemptID: attempt.AttemptID, Delivery: queued, Monitor: monitor.ToDomain(),
			Notification: &out, Template: template, IncludeTarget: intent.IncludeTarget, Regions: regions, Policy: set.HealthPolicy}
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	if declined {
		return nil, ports.ErrConflict
	}
	if grant == nil {
		return nil, ports.ErrNotFound
	}
	return grant, nil
}

// operativeRegionsTx renders nonsecret current source summaries for aggregate
// pages: member identity and last observed status only.
func operativeRegionsTx(ctx context.Context, tx bun.Tx, members []operativeMemberModel) ([]domain.AggregateSourceSummary, error) {
	out := make([]domain.AggregateSourceSummary, 0, len(members))
	for _, member := range members {
		var registration probeRegistrationModel
		summary := domain.AggregateSourceSummary{ProbeID: member.ProbeID}
		if member.ProbeID == domain.LocalProbeID {
			summary.Name, summary.Location = "local", "local"
		} else if err := tx.NewSelect().Model(&registration).Where("id = ?", member.ProbeID).Scan(ctx); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		} else {
			summary.Name, summary.Location = registration.Name, registration.Location
		}
		var state monitorProbeStateModel
		err := tx.NewSelect().Model(&state).Where("monitor_id = ? AND probe_id = ?", member.MonitorID, member.ProbeID).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			summary.Status = domain.Status(state.Status)
		}
		out = append(out, summary)
	}
	return out, nil
}

// FinishAggregateDelivery records one fenced result for leased aggregate work
// with the existing outbox effect contract: only the current, unexpired attempt
// may complete, an identical retry of a completed receipt is a no-op, and
// conflicting or superseded receipts fail. Provider-attempt outcomes are
// journaled separately through SourceProviderAttemptJournal; this method never
// fabricates one.
func (r *OperativeDeliveryStore) FinishAggregateDelivery(ctx context.Context, claim domain.DeliveryClaim, result domain.DeliveryResult) error {
	if r == nil || r.db == nil || !domain.ValidHubID(claim.DeliveryID) ||
		(claim.ProbeID != domain.LocalProbeID && !validRemoteProbeID(claim.ProbeID)) ||
		claim.Attempt < 1 || claim.LeaseToken == "" || len(claim.LeaseToken) > 64 {
		return domain.ErrValidation
	}
	if err := validateDeliveryResult(result); err != nil {
		return err
	}
	result.At = outboxTime(result.At)
	if !result.RetryAt.IsZero() {
		result.RetryAt = outboxTime(result.RetryAt)
		if !result.RetryAt.After(result.At) {
			return domain.ErrValidation
		}
	}
	return probeCommandError(ctx, runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var row operativeAggregateDeliveryModel
		if err := tx.NewSelect().Model(&row).Where("delivery_id = ?", claim.DeliveryID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return err
		}
		if row.PrimaryProbeID != claim.ProbeID || row.Attempt != claim.Attempt || row.LeaseToken != claim.LeaseToken {
			return ports.ErrConflict
		}
		if row.Status != domain.DeliveryStatusLeased {
			if operativeSameResult(row, result) {
				return nil
			}
			return ports.ErrConflict
		}
		if row.LeasedAt == nil || row.LeaseUntil == nil || result.At.Before(row.LeasedAt.UTC()) || !result.At.Before(row.LeaseUntil.UTC()) {
			return ports.ErrConflict
		}
		outcomeAt := result.At
		if _, err := tx.NewUpdate().Model((*operativeAggregateDeliveryModel)(nil)).
			Set("status = ?", result.Status).
			Set("outcome_at = ?", outcomeAt).
			Set("error_code = ?", result.ErrorCode).
			Set("lease_until = NULL").
			Set("available_at = ?", availableAtFor(result)).
			Where("delivery_id = ? AND attempt = ?", row.DeliveryID, row.Attempt).Exec(ctx); err != nil {
			return err
		}
		if result.Status == domain.DeliveryStatusRetrying {
			return nil // The intent stays pending until a terminal outcome.
		}
		var finalStatus string
		reason := ""
		switch result.Status {
		case domain.DeliveryStatusSent:
			finalStatus = "sent"
		case domain.DeliveryStatusFailed:
			finalStatus = "failed"
		case domain.DeliveryStatusSuperseded:
			finalStatus = "superseded"
			reason = "superseded"
		default:
			return domain.ErrValidation
		}
		q := tx.NewUpdate().Model((*operativeAggregateIntentModel)(nil)).Set("status = ?", finalStatus).Set("finalized_at = ?", outcomeAt)
		if reason != "" {
			q = q.Set("reason = ?", reason)
		}
		_, err := q.Where("intent_id = ? AND status = ?", row.IntentID, "pending").Exec(ctx)
		return err
	}))
}

func availableAtFor(result domain.DeliveryResult) time.Time {
	if result.Status == domain.DeliveryStatusRetrying {
		return outboxTime(result.RetryAt)
	}
	return outboxTime(result.At)
}

// operativeSameResult reports whether a completed receipt is an identical
// retry, which must be a no-op instead of a conflict.
func operativeSameResult(row operativeAggregateDeliveryModel, result domain.DeliveryResult) bool {
	return row.Status == result.Status && row.ErrorCode == result.ErrorCode &&
		row.OutcomeAt != nil && row.OutcomeAt.Equal(result.At) &&
		(result.Status != domain.DeliveryStatusRetrying || row.AvailableAt.Equal(outboxTime(result.RetryAt)))
}
