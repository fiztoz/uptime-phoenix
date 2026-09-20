package repository

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EscalationOutboxStore atomically transfers a claimed step into provider work.
// It shares the activation authority transaction with source-graph validation.
type EscalationOutboxStore struct {
	db      *bun.DB
	encoder ports.LocalProbeConfigEncoder
}

// NewEscalationOutboxStore constructs the MariaDB/SQLite escalation writer.
func NewEscalationOutboxStore(db *bun.DB, encoder ports.LocalProbeConfigEncoder) *EscalationOutboxStore {
	return &EscalationOutboxStore{db: db, encoder: encoder}
}

var _ ports.EscalationOutboxRepository = (*EscalationOutboxStore)(nil)

// CommitEscalationStep verifies current configuration, lifecycle and claim before
// atomically recording deliveries and progress. Provider I/O never runs here.
func (r *EscalationOutboxStore) CommitEscalationStep(ctx context.Context, plan domain.EscalationStepCommit) (bool, error) {
	if plan.EscalationID <= 0 || plan.ClaimToken == "" || plan.ExpectedStep <= 0 || plan.ConfigRevision <= 0 || plan.At.IsZero() {
		return false, domain.ErrValidation
	}
	committed := false
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		committed = false
		applied, err := readAppliedLocalTx(ctx, tx, r.encoder)
		if err != nil {
			return err
		}
		if applied.Revision != plan.ConfigRevision {
			return ports.ErrConflict
		}
		var state AlertEscalationModel
		q := tx.NewSelect().Model(&state).Where("id = ?", plan.EscalationID)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); err != nil {
			return probeRegistryError(err)
		}
		if state.Status != domain.EscalationStatePending || state.NextStep != plan.ExpectedStep ||
			state.LeaseOwner == nil || *state.LeaseOwner != plan.ClaimToken ||
			state.LeaseUntil == nil || !plan.At.Before(*state.LeaseUntil) || state.NextRunAt.After(plan.At) {
			return nil
		}
		var alert AlertModel
		q = tx.NewSelect().Model(&alert).Where("id = ?", state.AlertID)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); err != nil {
			return probeRegistryError(err)
		}
		if alert.Status != domain.AlertStatusFiring || alert.ProbeID != domain.LocalProbeID || alert.MonitorID != state.MonitorID {
			return finishEscalationTx(ctx, tx, &state, domain.EscalationStateCanceled, plan.At)
		}
		if err := requireActiveAssignment(ctx, tx, state.MonitorID, alert.ProbeID, alert.AssignmentGeneration); err != nil {
			return err
		}
		attached := false
		for _, a := range applied.Assignments {
			if a.Monitor != nil && a.Monitor.ID == state.MonitorID && a.Generation == alert.AssignmentGeneration &&
				a.EscalationPolicyID != nil && *a.EscalationPolicyID == state.PolicyID {
				attached = true
				break
			}
		}
		var step, next *domain.EscalationStep
		for _, policy := range applied.Policies {
			if policy == nil || policy.ID != state.PolicyID || !policy.Enabled {
				continue
			}
			for i := range policy.Steps {
				if policy.Steps[i].StepOrder == state.NextStep {
					step = &policy.Steps[i]
					if i+1 < len(policy.Steps) {
						next = &policy.Steps[i+1]
					}
					break
				}
			}
		}
		if !attached || step == nil {
			return finishEscalationTx(ctx, tx, &state, domain.EscalationStateCanceled, plan.At)
		}
		if !slices.Equal(step.NotificationIDs, plan.NotificationIDs) {
			return ports.ErrConflict
		}
		if next == nil {
			if plan.NextStep != 0 || !plan.NextRunAt.IsZero() {
				return domain.ErrValidation
			}
		} else if plan.NextStep != next.StepOrder || !plan.NextRunAt.Equal(plan.At.Add(time.Duration(next.WaitMinutes)*time.Minute)) {
			return domain.ErrValidation
		}
		incident, err := lockDeliveryIncident(ctx, tx, alert.SourceAlertID)
		if err != nil {
			return err
		}
		if incident.Status != domain.AlertStatusFiring || (incident.AssignmentGeneration == nil || *incident.AssignmentGeneration != alert.AssignmentGeneration) {
			return finishEscalationTx(ctx, tx, &state, domain.EscalationStateCanceled, plan.At)
		}
		// Current state survives telemetry pruning. Do not fabricate a DOWN
		// sample while maintenance or a retry window is active.
		var observation monitorProbeStateModel
		err = tx.NewSelect().Model(&observation).
			Where("monitor_id = ? AND probe_id = ? AND assignment_generation = ?", state.MonitorID, alert.ProbeID, alert.AssignmentGeneration).Scan(ctx)
		if err != nil {
			return probeRegistryError(err)
		}
		if domain.Status(observation.Status) != domain.StatusDown {
			return nil
		}

		for _, id := range plan.NotificationIDs {
			enabled := false
			for _, n := range applied.Notifications {
				if n != nil && n.ID == id && n.Active {
					enabled = true
					break
				}
			}
			if !enabled {
				continue
			}
			deliveryID, err := uuid.NewRandom()
			if err != nil {
				return err
			}
			row := deliveryIntentModel{
				DeliveryID: deliveryID.String(), SourceAlertID: incident.SourceAlertID,
				SourceTransitionVersion: incident.TransitionVersion, ProbeID: alert.ProbeID,
				NotificationID: id, NotificationVersion: applied.Revision,
				EventKind: domain.DeliveryEventStatusChange, EscalationPolicyID: state.PolicyID, EscalationStep: state.NextStep,
				MonitorID: state.MonitorID, AssignmentGeneration: alert.AssignmentGeneration,
				StreamID: observation.StreamID, SourceSeq: observation.Seq, ConfigRevision: observation.ConfigRevision,
				CheckStatus: int(domain.StatusDown), CheckOutput: incident.Reason, ObservedAt: outboxTime(observation.ObservedAt),
				IncidentStatus: domain.AlertStatusFiring, StartedAt: outboxTime(incident.StartedAt),
				AvailableAt: outboxTime(plan.At), Status: domain.DeliveryStatusPending, CreatedAt: outboxTime(plan.At),
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		if next == nil {
			if err := finishEscalationTx(ctx, tx, &state, domain.EscalationStateDone, plan.At); err != nil {
				return err
			}
		} else {
			_, err := tx.NewUpdate().Model(&state).
				Set("next_step = ?", plan.NextStep).Set("next_run_at = ?", outboxTime(plan.NextRunAt)).
				Set("lease_owner = NULL").Set("lease_until = NULL").Set("updated_at = ?", outboxTime(plan.At)).WherePK().Exec(ctx)
			if err != nil {
				return err
			}
		}
		committed = true
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrValidation) {
			return false, err
		}
		return false, domain.ErrInternal // Decoder/SQL errors must not disclose channel credentials.
	}
	return committed, nil
}

func finishEscalationTx(ctx context.Context, tx bun.Tx, state *AlertEscalationModel, status string, at time.Time) error {
	_, err := tx.NewUpdate().Model(state).Set("status = ?", status).
		Set("lease_owner = NULL").Set("lease_until = NULL").Set("updated_at = ?", outboxTime(at)).WherePK().Exec(ctx)
	return err
}
