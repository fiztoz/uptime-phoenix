package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ErrLocalDeliverySuppressed reports a refused local availability send or
// enqueue: a monitor-wide operative aggregate source hold is active. It is
// suppression, not corruption, and must never be answered with a fabricated
// completion. Main-owned local heartbeat/escalation paths call the Tx helpers
// below before enqueueing availability work.
var ErrLocalDeliverySuppressed = errors.New("local availability delivery suppressed by operative source hold")

const operativeMaxProviderBudget = 15 * time.Minute

// localCutoverEffectTx commits the hub-local source effect for one cutover
// command in the same transaction as its immutable kind receipt. The hold is
// persisted BEFORE the provider-attempt check: a busy/ambiguous refusal keeps
// the hold and is never recorded as success.
func (r *OperativeDeliveryStore) localCutoverEffectTx(ctx context.Context, tx bun.Tx, _ string, barrier operativeBarrierModel, member *operativeMemberModel, kind string, now time.Time) (string, string, error) {
	if member.ProbeID != domain.LocalProbeID || member.MonitorID != barrier.MonitorID {
		return "", "", ports.ErrConflict
	}
	at := now.UTC().Truncate(time.Microsecond)
	switch kind {
	case domain.DeliveryQuiesceKind:
		var gate operativeGateModel
		err := tx.NewSelect().Model(&gate).Where("probe_id = ? AND monitor_id = ? AND barrier_id = ?", domain.LocalProbeID, barrier.MonitorID, barrier.BarrierID).Scan(ctx)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			gate = operativeGateModel{ProbeID: domain.LocalProbeID, MonitorID: barrier.MonitorID, BarrierID: barrier.BarrierID,
				Mode: barrier.AlertDelivery, State: "held", GrantedAt: at}
			if _, err := tx.NewInsert().Model(&gate).Exec(ctx); err != nil {
				return "", "", err
			}
		case err != nil:
			return "", "", err
		default:
			// The hold is immutable across retries; a contradiction is corruption.
			if gate.Mode != barrier.AlertDelivery || gate.GrantedAt.IsZero() || (gate.State != "held" && gate.State != "released") {
				return "", "", ports.ErrConflict
			}
		}
		if gate.State == "released" {
			return "rejected", "delivery_barrier_released", nil
		}
		unresolved, err := operativeUnresolvedAttemptsTx(ctx, tx, barrier.MonitorID)
		if err != nil {
			return "", "", err
		}
		if unresolved {
			return "rejected", operativeBusyCode, nil
		}
		return "applied", domain.DeliveryQuiescedCode, nil
	case domain.DeliveryApplyKind:
		var gate operativeGateModel
		err := tx.NewSelect().Model(&gate).Where("probe_id = ? AND monitor_id = ? AND barrier_id = ? AND state = ?", domain.LocalProbeID, barrier.MonitorID, barrier.BarrierID, "held").Scan(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", "", ports.ErrConflict // No exact quiesced gate, no application.
			}
			return "", "", err
		}
		if gate.Mode != barrier.AlertDelivery {
			return "", "", ports.ErrConflict
		}
		// Source authentication of the one captured configuration happens via the
		// caller's exact whole-graph drift check in this same transaction.
		row := operativeLocalApplicationModel{ProbeID: domain.LocalProbeID, ConfigRevision: member.ConfigRevision, MonitorID: barrier.MonitorID,
			BarrierID: barrier.BarrierID, AssignmentRevision: member.AssignmentRevision, Generation: member.Generation,
			SHA256: member.SHA256, StreamID: member.StreamID, AlertDelivery: barrier.AlertDelivery, AppliedAt: at}
		var stored operativeLocalApplicationModel
		err = tx.NewSelect().Model(&stored).Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND barrier_id = ?", row.ProbeID, row.ConfigRevision, row.MonitorID, row.BarrierID).Scan(ctx)
		if err == nil {
			if stored.BarrierID != row.BarrierID || stored.AssignmentRevision != row.AssignmentRevision || stored.Generation != row.Generation ||
				stored.SHA256 != row.SHA256 || stored.StreamID != row.StreamID || stored.AlertDelivery != row.AlertDelivery || stored.AppliedAt.IsZero() {
				return "", "", ports.ErrConflict
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		} else if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return "", "", err
		}
		return "applied", domain.DeliveryAppliedCode, nil
	case domain.DeliveryResumeKind:
		// Resume authenticates the stored barrier tuple and releases only this
		// barrier's hold. An old configuration may be released after replacement;
		// a replacement barrier's hold is never released here.
		result, err := tx.NewUpdate().Model((*operativeGateModel)(nil)).Set("state = ?", "released").Set("released_at = ?", at).
			Where("probe_id = ? AND monitor_id = ? AND barrier_id = ? AND state = ?", domain.LocalProbeID, barrier.MonitorID, barrier.BarrierID, "held").Exec(ctx)
		if err != nil {
			return "", "", err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return "", "", err
		}
		if n > 0 {
			return "applied", domain.DeliveryResumedCode, nil
		}
		var stored operativeGateModel
		err = tx.NewSelect().Model(&stored).Where("probe_id = ? AND monitor_id = ? AND barrier_id = ?", domain.LocalProbeID, barrier.MonitorID, barrier.BarrierID).Scan(ctx)
		if err == nil {
			if stored.State != "released" || stored.Mode != barrier.AlertDelivery || stored.ReleasedAt == nil {
				return "", "", ports.ErrConflict
			}
			return "already_applied", domain.DeliveryResumedCode, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", "", err
		}
		// Resume-before-quiesce is an effect, not a success-shaped no-op.
		// Its exact released tombstone rejects a late old quiesce forever.
		live, err := tx.NewSelect().Model((*operativeGateModel)(nil)).Where("probe_id = ? AND monitor_id = ? AND state = ?", domain.LocalProbeID, barrier.MonitorID, "held").Exists(ctx)
		if err != nil {
			return "", "", err
		}
		if live {
			return "", "", ports.ErrConflict
		}
		stored = operativeGateModel{ProbeID: domain.LocalProbeID, MonitorID: barrier.MonitorID, BarrierID: barrier.BarrierID, Mode: barrier.AlertDelivery, State: "released", GrantedAt: at, ReleasedAt: &at}
		if _, err := tx.NewInsert().Model(&stored).Exec(ctx); err != nil {
			return "", "", err
		}
		return "applied", domain.DeliveryResumedCode, nil
	default:
		return "", "", domain.ErrValidation
	}
}

// LocalAvailabilityEnqueueSuppressedTx reports whether a monitor-wide operative
// aggregate source hold blocks new regional availability provider starts and
// availability enqueue for the hub-local source. Main-owned local heartbeat
// recording must call this inside its own transaction before enqueueing
// availability work. A BOTH-mode hold retains regional work and never
// suppresses; config changes, expiry, disconnect or reset never erase the hold.
func LocalAvailabilityEnqueueSuppressedTx(ctx context.Context, tx bun.Tx, monitorID int64) (bool, error) {
	return localAvailabilitySuppressedTx(ctx, tx, monitorID)
}

// LocalAvailabilityEscalationSuppressedTx is the same monitor-wide hold check
// for main-owned escalation intent creation. Escalation intents are availability
// work: an aggregate hold blocks them exactly like new provider starts.
func LocalAvailabilityEscalationSuppressedTx(ctx context.Context, tx bun.Tx, monitorID int64) (bool, error) {
	return localAvailabilitySuppressedTx(ctx, tx, monitorID)
}

func localAvailabilitySuppressedTx(ctx context.Context, tx bun.Tx, monitorID int64) (bool, error) {
	if monitorID <= 0 {
		return false, domain.ErrValidation
	}
	count, err := tx.NewSelect().Table("operative_source_gates").ColumnExpr("COUNT(*)").
		Where("probe_id = ?", domain.LocalProbeID).Where("monitor_id = ?", monitorID).
		Where("state = ? AND mode = ?", "held", domain.AlertDeliveryAggregate).Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// SetOperativeSourceCapability records or withdraws one source's compiled
// strong capability attestation. The wire/main handshake integration invokes it
// from the exact advertised availability.delivery_cutover.v1 welcome opt-in
// (compiled executor + durable source gate + provider-attempt journal
// configured). A withdrawal on downgrade removes eligibility immediately; an
// absent new handshake never erases a stored attestation.
func (r *OperativeDeliveryStore) SetOperativeSourceCapability(ctx context.Context, session domain.ProbeReplaySession, supported bool) error {
	if r == nil || r.db == nil || r.protector == nil || !validRemoteProbeID(session.ProbeID) || !validStandbyUUID(session.OwnerID) || session.ConnectionGeneration < 1 {
		return domain.ErrValidation
	}
	probeID, connectionGeneration := session.ProbeID, session.ConnectionGeneration
	return probeCommandError(ctx, runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		authority, err := lockProbeSession(ctx, tx, session, r.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		now := authority.now
		finish := func(writeErr error) error {
			if writeErr != nil {
				return writeErr
			}
			final, err := lockProbeSession(ctx, tx, session, r.protector.KeyHash(session.HubID))
			if err != nil {
				return err
			}
			if final.now.Before(now) {
				return ports.ErrConflict
			}
			return nil
		}
		if !supported {
			_, err := tx.NewDelete().Model((*operativeCapabilityModel)(nil)).
				Where("probe_id = ? AND capability = ?", probeID, domain.DeliveryCutoverCapability).Exec(ctx)
			return finish(err)
		}
		row := operativeCapabilityModel{ProbeID: probeID, Capability: domain.DeliveryCutoverCapability,
			ConnectionGeneration: connectionGeneration, AttestedAt: now.UTC().Truncate(time.Microsecond)}
		var stored operativeCapabilityModel
		err = tx.NewSelect().Model(&stored).Where("probe_id = ?", probeID).Scan(ctx)
		if err == nil {
			if stored.Capability != "" && stored.Capability != domain.DeliveryCutoverCapability {
				return ports.ErrConflict
			}
			_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
			return finish(err)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
		return finish(err)
	}))
}

// AuthorizeLocalDelivery is the final DB transaction for one hub-local
// availability provider attempt. It rechecks the exact stored claim identity
// (delivery, attempt counter, lease proof), that the stored lease covers the
// whole provider budget, the entire authenticated source graph and its ACK, the
// current set binding and generation, the current stored regional facts, the
// monitor-wide source gate, the incident lifecycle and the channel binding,
// then journals one opaque attempt grant in the same transaction. The returned
// settings come from this single captured read, never a caller snapshot.
func (r *OperativeDeliveryStore) AuthorizeLocalDelivery(ctx context.Context, claim domain.DeliveryClaim, config domain.ProbeActiveConfig, budget time.Duration) (*domain.LocalProviderGrant, error) {
	if r == nil || r.db == nil || r.encoder == nil || claim.ProbeID != domain.LocalProbeID ||
		claim.Attempt < 1 || claim.LeaseToken == "" || len(claim.LeaseToken) > 64 ||
		!domain.ValidHubID(claim.DeliveryID) || budget <= 0 || budget > operativeMaxProviderBudget ||
		!domain.ValidProbeActiveConfig(config) || config.ProbeID != domain.LocalProbeID {
		return nil, domain.ErrValidation
	}
	var grant *domain.LocalProviderGrant
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		grant = nil
		definition, err := readAppliedLocalTx(ctx, tx, r.encoder)
		if err != nil {
			return err
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
			return err
		}
		var receipt probeConfigAppliedReceiptModel
		if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", domain.LocalProbeID, active.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrConflict
			}
			return err
		}
		// ACK: active pointer, durable receipt and the caller's captured config
		// must be the same exact committed state.
		if active.HubID != config.HubID || active.Revision != config.Revision || active.SHA256 != config.SHA256 ||
			!active.AppliedAt.Equal(config.AppliedAt) || active.AssignmentCount != config.AssignmentCount ||
			len(definition.Assignments) != config.AssignmentCount || definition.Revision != config.Revision ||
			receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
			return ports.ErrConflict
		}
		var errGrant error
		grant, errGrant = r.grantLocalAttemptTx(ctx, tx, claim, config, definition, budget)
		return errGrant
	})
	if err != nil {
		if errors.Is(err, ErrLocalDeliverySuppressed) {
			return nil, ErrLocalDeliverySuppressed
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err // Typed conflicts keep their bounded context; callers use errors.Is.
	}
	return grant, nil
}
