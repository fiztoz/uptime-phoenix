package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// grantLocalAttemptTx performs the second half of the local final authorization
// inside the caller's fenced transaction: stored claim identity, lease budget,
// set/generation, current regional facts, monitor-wide gate, incident lifecycle
// and channel binding, then the single provider-attempt grant.
func (r *OperativeDeliveryStore) grantLocalAttemptTx(ctx context.Context, tx bun.Tx, claim domain.DeliveryClaim, config domain.ProbeActiveConfig, definition *domain.LocalProbeConfigDefinition, budget time.Duration) (*domain.LocalProviderGrant, error) {
	var row deliveryIntentModel
	if err := tx.NewSelect().Model(&row).Where("delivery_id = ? AND probe_id = ?", claim.DeliveryID, domain.LocalProbeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err
	}
	now, err := replayDatabaseTime(ctx, tx)
	if err != nil {
		return nil, err
	}
	// Exact stored claim identity; the stored lease must cover the whole
	// provider budget (authorization plus provider I/O share one deadline).
	// The stored lease (attempt + claim proof + deadline) must cover the whole
	// provider budget. The lease window may have been written by the caller's
	// claim clock, so only the DB-clock budget bound is compared here; a mixed
	// lower bound would silently shift the window under clock skew.
	if now.Before(row.AvailableAt.UTC()) {
		return nil, ports.ErrConflict
	}
	if row.Attempt != claim.Attempt || row.LeaseToken == nil || *row.LeaseToken != claim.LeaseToken || row.Status != domain.DeliveryStatusLeased ||
		row.LeaseUntil == nil || !now.Add(budget).Before(row.LeaseUntil.UTC()) {
		return nil, fmt.Errorf("local claim lease fence: %w", ports.ErrConflict)
	}
	if row.ConfigRevision != config.Revision || row.StreamID != domain.LocalStreamID {
		return nil, fmt.Errorf("local claim document binding: %w", ports.ErrConflict)
	}
	set, err := readProbeAssignments(ctx, tx, row.MonitorID)
	if err != nil {
		return nil, err
	}
	var member *domain.ProbeAssignment
	for i := range set.Assignments {
		if set.Assignments[i].ProbeID == domain.LocalProbeID {
			member = &set.Assignments[i]
			break
		}
	}
	if member == nil || member.Generation != row.AssignmentGeneration {
		return nil, fmt.Errorf("local claim assignment generation: %w", ports.ErrConflict)
	}
	// Current stored regional facts for this exact tuple.
	fact, ok := localFactForMonitor(definition, row.MonitorID)
	if ok && (fact.AlertDelivery != domain.AlertDeliveryRegional ||
		fact.ConfigRevision != config.Revision || fact.SHA256 != config.SHA256 || fact.StreamID != domain.LocalStreamID) {
		return nil, fmt.Errorf("local claim regional fact: %w", ports.ErrConflict)
	}
	// Gate: monitor-wide, never discarded by a config change.
	suppressed, err := localAvailabilitySuppressedTx(ctx, tx, row.MonitorID)
	if err != nil {
		return nil, err
	}
	if suppressed {
		return nil, ErrLocalDeliverySuppressed
	}
	// Lifecycle: the exact source incident this delivery belongs to.
	var incident probeIncidentModel
	if err := tx.NewSelect().Model(&incident).Where("source_alert_id = ?", row.SourceAlertID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}
	if incident.Scope != string(domain.IncidentScopeRegional) || incident.SubjectKind != domain.IncidentSubjectAvailability ||
		incident.MonitorID == nil || *incident.MonitorID != row.MonitorID || incident.TransitionVersion != row.SourceTransitionVersion ||
		(domain.Status(row.CheckStatus) == domain.StatusDown && (incident.Status != domain.AlertStatusFiring || incident.ResolvedAt != nil || incident.AckedAt != nil)) ||
		(domain.Status(row.CheckStatus) == domain.StatusUp && (incident.Status != domain.AlertStatusResolved || incident.ResolvedAt == nil)) {
		return nil, ports.ErrConflict
	}
	// Channel: the notification must still be active and attached to this
	// monitor in the authenticated graph.
	var notification NotificationModel
	if err := tx.NewSelect().Model(&notification).Where("id = ?", row.NotificationID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}
	if !notification.Active {
		return nil, fmt.Errorf("local claim channel inactive: %w", ports.ErrConflict)
	}
	attached := false
	for _, assignment := range definition.Assignments {
		if assignment.Monitor == nil || assignment.Monitor.ID != row.MonitorID {
			continue
		}
		for _, link := range assignment.NotificationLinks {
			if row.EscalationStep == 0 && link.NotificationID == row.NotificationID {
				attached = true
				break
			}
		}
		if row.EscalationStep > 0 && assignment.EscalationPolicyID != nil && *assignment.EscalationPolicyID == row.EscalationPolicyID {
			for _, policy := range definition.Policies {
				if policy == nil || !policy.Enabled || policy.ID != row.EscalationPolicyID {
					continue
				}
				for _, step := range policy.Steps {
					if step.StepOrder != row.EscalationStep {
						continue
					}
					for _, id := range step.NotificationIDs {
						if id == row.NotificationID {
							attached = true
						}
					}
				}
			}
		}
	}
	if !attached {
		return nil, fmt.Errorf("local claim channel binding: %w", ports.ErrConflict)
	}
	finalNow, err := replayDatabaseTime(ctx, tx)
	if err != nil {
		return nil, err
	}
	if finalNow.Before(now) || !finalNow.Add(budget).Before(row.LeaseUntil.UTC()) {
		return nil, ports.ErrConflict
	}
	now = finalNow
	attemptID, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	attempt := operativeAttemptModel{AttemptID: attemptID.String(), Scope: "source", MonitorID: row.MonitorID,
		ProbeID: domain.LocalProbeID, DeliveryID: row.DeliveryID, ClaimAttempt: claim.Attempt,
		NotificationID: row.NotificationID, BudgetUntil: now.Add(budget), State: string(domain.ProviderAttemptInFlight),
		CreatedAt: now.UTC().Truncate(time.Microsecond)}
	if !validStandbyUUID(attempt.AttemptID) {
		return nil, ports.ErrConflict
	}
	// One provider budget per claim attempt: a duplicate grant is refused
	// rather than silently doubling external I/O.
	if _, err := tx.NewInsert().Model(&attempt).Exec(ctx); err != nil {
		return nil, ports.ErrConflict
	}
	return &domain.LocalProviderGrant{AttemptID: attempt.AttemptID, Delivery: row.queued(), Config: definition}, nil
}

// FinishSourceProviderAttempt completes only the exact admitted provider grant.
// A matching release is valid even after lease/budget expiry and never affects a
// replacement grant. The recorded outcome is immutable: only succeeded and
// ambiguous (or a genuinely not-started refusal) may be journaled, an identical repeat is a no-op, and any
// contradictory repeat is a conflict. An unfinished (crashed) grant stays
// in_flight and blocks quiescence/drain; nothing here invents completion.
func (r *OperativeDeliveryStore) FinishSourceProviderAttempt(ctx context.Context, attemptID string, state domain.ProviderAttemptState) error {
	if r == nil || r.db == nil || !validStandbyUUID(attemptID) ||
		(state != domain.ProviderAttemptSucceeded && state != domain.ProviderAttemptAmbiguous && state != domain.ProviderAttemptNotStarted) {
		return domain.ErrValidation
	}
	return probeCommandError(ctx, runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var attempt operativeAttemptModel
		if err := tx.NewSelect().Model(&attempt).Where("attempt_id = ?", attemptID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return err
		}
		if attempt.State != string(domain.ProviderAttemptInFlight) {
			if attempt.State == string(state) {
				return nil // Idempotent exact repeat; expiry never revives a grant.
			}
			return ports.ErrConflict // Immutable outcome; a replacement is a new grant.
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		at := now.UTC().Truncate(time.Microsecond)
		// Exact completion is identity-authorized, not clock-authorized. Keep
		// the actual DB observation even if its wall clock stepped backward;
		// never fabricate an I/O time or strand a returned provider attempt.
		attempt.State = string(state)
		attempt.FinishedAt = &at
		_, err = tx.NewUpdate().Model(&attempt).Column("state", "finished_at").WherePK().Exec(ctx)
		return err
	}))
}
