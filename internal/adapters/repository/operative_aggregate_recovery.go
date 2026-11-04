package repository

import (
	"context"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// reconcileAggregateDeliveryTx runs under the claim transaction's row lock and
// DB clock, only for due retries or expired claims with no still-running grant.
// A lost delivery receipt is not permission to repeat provider I/O. Preserve the
// journal (including unfinished grants) and record the queue's recovery decision
// at the actual observation time, never a fabricated provider completion time.
func reconcileAggregateDeliveryTx(ctx context.Context, tx bun.Tx, row operativeAggregateDeliveryModel, now time.Time) (bool, error) {
	var attempts []operativeAttemptModel
	if err := tx.NewSelect().Model(&attempts).
		Where("delivery_id = ? AND scope = ?", row.DeliveryID, "aggregate").
		OrderExpr("claim_attempt DESC, attempt_id DESC").Scan(ctx); err != nil {
		return false, err
	}
	var intent operativeAggregateIntentModel
	if err := tx.NewSelect().Model(&intent).Where("intent_id = ?", row.IntentID).Scan(ctx); err != nil {
		return false, err
	}
	if intent.IncidentID != row.IncidentID || intent.MonitorID != row.MonitorID || intent.BarrierID != row.BarrierID ||
		intent.NotificationID != row.NotificationID || intent.TransitionVersion != row.TransitionVersion {
		return false, ports.ErrConflict
	}
	status, code := "", ""
	unknown, succeeded := false, false
	for _, attempt := range attempts {
		if attempt.ClaimAttempt > row.Attempt || attempt.MonitorID != row.MonitorID || attempt.NotificationID != row.NotificationID || attempt.ProbeID != row.PrimaryProbeID {
			return false, ports.ErrConflict
		}
		switch domain.ProviderAttemptState(attempt.State) {
		case domain.ProviderAttemptInFlight:
			if attempt.BudgetUntil.After(now.Add(-operativeAttemptSettleGrace)) {
				return true, nil
			}
			unknown = true
		case domain.ProviderAttemptSucceeded:
			succeeded = true
		}
	}
	// Positive provider evidence is stronger than an absent queue receipt, even
	// when an older version already admitted more than one attempt for this row.
	if succeeded {
		status = string(domain.DeliveryStatusSent)
	} else if unknown {
		status, code = string(domain.DeliveryStatusFailed), "provider_outcome_unknown"
	} else {
		var incident operativeAggregateIncidentModel
		if err := tx.NewSelect().Model(&incident).Where("incident_id = ?", row.IncidentID).Scan(ctx); err != nil {
			return false, err
		}
		var state operativeStateModel
		if err := tx.NewSelect().Model(&state).Where("monitor_id = ?", row.MonitorID).Scan(ctx); err != nil {
			return false, err
		}
		obsolete := intent.Status != "pending" || incident.TransitionVersion != row.TransitionVersion ||
			state.Phase != operativePhaseActive || state.BarrierID == nil || *state.BarrierID != row.BarrierID ||
			(intent.Kind == "recovery" && (incident.Status != "resolved" || incident.CloseReason != "recovered")) ||
			(intent.Kind != "recovery" && incident.Status != "open")
		if obsolete {
			status = string(domain.DeliveryStatusSuperseded)
		} else if len(attempts) > 0 && attempts[0].State == string(domain.ProviderAttemptAmbiguous) {
			// A committed retry remains valid if its next worker only claimed
			// the row, then died before authorization. The old outcome receipt
			// survives claiming, and no grant exists for the newer claim.
			latest := attempts[0]
			explicitRetry := row.Status == domain.DeliveryStatusRetrying ||
				(row.Status == domain.DeliveryStatusLeased && row.OutcomeAt != nil && latest.ClaimAttempt < row.Attempt)
			if !explicitRetry || row.Attempt >= 5 {
				// Losing the retry decision is not implicit retry authorization.
				status, code = string(domain.DeliveryStatusFailed), "provider_outcome_unknown"
			}
		}
	}
	if status == "" {
		return false, nil
	}
	at := now.UTC().Truncate(time.Microsecond)
	if _, err := tx.NewUpdate().Model((*operativeAggregateDeliveryModel)(nil)).
		Set("status = ?", status).Set("error_code = ?", code).Set("outcome_at = ?", at).
		Set("lease_until = NULL").
		Where("delivery_id = ? AND attempt = ? AND status = ?", row.DeliveryID, row.Attempt, row.Status).Exec(ctx); err != nil {
		return false, err
	}
	reason := ""
	if status == string(domain.DeliveryStatusSuperseded) {
		reason = "superseded"
	}
	// A drain/recovery already finalized the old intent. Never rewrite it.
	if _, err := tx.NewUpdate().Model((*operativeAggregateIntentModel)(nil)).
		Set("status = ?", status).Set("reason = ?", reason).Set("finalized_at = ?", at).
		Where("intent_id = ? AND status = ?", row.IntentID, "pending").Exec(ctx); err != nil {
		return false, err
	}
	return true, nil
}
