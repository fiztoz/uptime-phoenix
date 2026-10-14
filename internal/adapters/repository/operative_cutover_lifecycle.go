package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func operativeBarrierTx(ctx context.Context, tx bun.Tx, barrierID string) (operativeBarrierModel, []operativeMemberModel, error) {
	var barrier operativeBarrierModel
	if err := tx.NewSelect().Model(&barrier).Where("barrier_id = ?", barrierID).Scan(ctx); err != nil {
		return barrier, nil, err
	}
	var members []operativeMemberModel
	if err := tx.NewSelect().Model(&members).Where("barrier_id = ?", barrierID).Order("probe_id ASC").Scan(ctx); err != nil {
		return barrier, nil, err
	}
	if len(members) == 0 {
		return barrier, nil, ports.ErrConflict
	}
	return barrier, members, nil
}

// operativeCommandState classifies one issued command from its immutable
// receipt. Rearmable covers rejected-busy and expired only; any other refusal
// is an explicit failure and blocks the barrier.
type operativeCommandState struct {
	Pending   bool
	Success   bool
	Rearmable bool
	Blocked   bool
}

func (m operativeMemberModel) application(hub string, monitorID int64, mode domain.AlertDelivery) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: hub, ProbeID: m.ProbeID},
		MonitorID:          monitorID,
		AssignmentRevision: m.AssignmentRevision,
		Generation:         m.Generation,
		ConfigRevision:     m.ConfigRevision,
		SHA256:             m.SHA256,
		StreamID:           m.StreamID,
		AlertDelivery:      mode,
	}
}

// preparingTx builds one complete immutable barrier from current captured
// evidence: exact retained configuration/ACK/binding/regional fact and the
// matching shared nonce-bound successful preparation for every member, plus the
// compiled strong capability for remote members. Missing is pending; internal
// contradiction fails explicitly.
func (r *OperativeDeliveryStore) preparingTx(ctx context.Context, tx bun.Tx, state *operativeStateModel, hub string, now time.Time) (bool, error) {
	set, err := readProbeAssignments(ctx, tx, state.MonitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	mode, ok := domain.CanonicalAlertDelivery(set.AlertDelivery)
	if !ok || (mode != domain.AlertDeliveryAggregate && mode != domain.AlertDeliveryBoth) {
		return false, nil // Regional default: nothing to activate, ever.
	}
	round, found, err := operativePreparationRoundTx(ctx, tx, hub, state.MonitorID, set.Revision, mode)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	type prepared struct {
		evidence operativeMemberEvidence
		prepID   string
	}
	candidates := make([]prepared, 0, len(set.Assignments))
	for _, assignment := range set.Assignments {
		ev, err := r.readOperativeMemberEvidenceTx(ctx, tx, hub, state.MonitorID, assignment, mode)
		if err != nil {
			return false, err
		}
		if ev.Evidence == operativeEvidenceCorrupt {
			return true, operativeBlockTx(state, now, "member_evidence_conflict")
		}
		if ev.Evidence != operativeEvidenceComplete || !ev.Capable {
			return false, nil // Missing/unsupported evidence stays pending.
		}
		// Pure cutover semantics: an unbound or stale document blocks even when
		// later application evidence would look current.
		if ev.Assignment == nil || ev.Assignment.Generation != assignment.Generation ||
			ev.Assignment.AssignmentRevision != set.Revision || ev.Assignment.AlertDelivery != mode {
			return false, nil
		}
		prepID, prepState, err := operativeMemberPreparedTx(ctx, tx, ev, round)
		if err != nil {
			return false, err
		}
		if prepState == operativeEvidenceCorrupt {
			return true, operativeBlockTx(state, now, "preparation_evidence_conflict")
		}
		if prepState != operativeEvidenceComplete {
			return false, nil
		}
		candidates = append(candidates, prepared{evidence: ev, prepID: prepID})
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return false, err
	}
	created := now.UTC().Truncate(time.Microsecond)
	barrier := operativeBarrierModel{BarrierID: id.String(), MonitorID: state.MonitorID, HubID: hub, AlertDelivery: mode,
		AssignmentRevision: set.Revision, RoundID: round.RoundID, RequestRound: round.RequestRound, CreatedAt: created}
	if !validStandbyUUID(barrier.BarrierID) {
		return false, ports.ErrConflict
	}
	if _, err := tx.NewInsert().Model(&barrier).Exec(ctx); err != nil {
		return false, err
	}
	members := make([]operativeMemberModel, len(candidates))
	for i, c := range candidates {
		ev := c.evidence
		members[i] = operativeMemberModel{BarrierID: barrier.BarrierID, ProbeID: ev.ProbeID, MonitorID: state.MonitorID,
			Generation: ev.Generation, AssignmentRevision: ev.AssignmentRevision, ConfigRevision: ev.ConfigRevision,
			SHA256: ev.SHA256, StreamID: ev.StreamID, AlertDelivery: mode, PreparationCommandID: c.prepID}
		if !domain.ValidProbeDeliveryModeApplication(members[i].application(hub, state.MonitorID, mode)) {
			return false, ports.ErrConflict
		}
		if _, err := tx.NewInsert().Model(&members[i]).Exec(ctx); err != nil {
			return false, err
		}
	}
	// QUIESCING: the barrier and its exact all-member tuples are appended, then
	// protected delivery.quiesce commands are issued. The local source's hold is
	// persisted before its attempt check inside the same command commit.
	for i := range members {
		issued, err := r.issueCutoverCommandTx(ctx, tx, hub, barrier, &members[i], domain.DeliveryQuiesceKind, now)
		if err != nil {
			return false, err
		}
		if !validStandbyUUID(issued) {
			return false, ports.ErrConflict
		}
	}
	barrierID := barrier.BarrierID
	state.BarrierID = &barrierID
	state.Phase = operativePhaseQuiescing
	state.AlertDelivery = mode
	state.AssignmentRevision = set.Revision
	state.Reason = ""
	return true, nil
}

// issueCutoverCommandTx appends one immutable protected command request. Local
// members commit their source effect (hold/application/release) and kind
// receipt in the same transaction; remote members get a sealed probe_commands
// row whose command ID is always fresh (rearms never rewrite old outcomes).
func (r *OperativeDeliveryStore) issueCutoverCommandTx(ctx context.Context, tx bun.Tx, hub string, barrier operativeBarrierModel, member *operativeMemberModel, kind string, now time.Time) (string, error) {
	if domain.DeliveryCutoverSuccessCode(kind) == "" {
		return "", domain.ErrValidation
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	commandID := id.String()
	created := now.UTC().Truncate(time.Microsecond)
	expires := created.Add(operativeCommandLifetime)
	logRow := operativeCommandModel{CommandID: commandID, BarrierID: barrier.BarrierID, MonitorID: barrier.MonitorID,
		ProbeID: member.ProbeID, Kind: kind, IssuedAt: created, ExpiresAt: expires}
	if member.ProbeID == domain.LocalProbeID {
		status, code, err := r.localCutoverEffectTx(ctx, tx, hub, barrier, member, kind, now)
		if err != nil {
			return "", err
		}
		logRow.OutcomeStatus = status
		logRow.OutcomeCode = code
		at := created
		logRow.OutcomeAt = &at
	} else {
		payload, meta, err := r.sealCutoverCommand(ctx, hub, barrier, member, kind, commandID, created, expires)
		if err != nil {
			return "", err
		}
		if err := reserveCommandCapacity(ctx, tx, member.ProbeID, now, 1, len(payload)); err != nil {
			clear(payload)
			return "", err
		}
		row := newPendingCommandRow(domain.ProtectedProbeCommand{ProbeCommandMetadata: meta, ProtectedPayload: payload, RequestedBy: 0}, now)
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			clear(payload)
			return "", err
		}
	}
	if _, err := tx.NewInsert().Model(&logRow).Exec(ctx); err != nil {
		return "", err
	}
	switch kind {
	case domain.DeliveryQuiesceKind:
		member.QuiesceCommandID = &commandID
	case domain.DeliveryApplyKind:
		member.ApplyCommandID = &commandID
	case domain.DeliveryResumeKind:
		member.ResumeCommandID = &commandID
	}
	_, err = tx.NewUpdate().Model(member).Column("quiesce_command_id", "apply_command_id", "resume_command_id").WherePK().Exec(ctx)
	return commandID, err
}

// sealCutoverCommand builds and verifies one closed protected request through
// the shared codec before sealing. The decoded round trip fixes the payload
// hash exactly like delivery preparation issuance. Metadata bounds are checked
// here directly because shared metadata validation does not yet accept the
// three new kinds (reported contract gap; wire owns that validation edit).
func (r *OperativeDeliveryStore) sealCutoverCommand(ctx context.Context, hub string, barrier operativeBarrierModel, member *operativeMemberModel, kind, commandID string, created, expires time.Time) ([]byte, domain.ProbeCommandMetadata, error) {
	request := domain.DeliveryCutoverCommand{
		ProbeDeliveryModeApplication: member.application(hub, barrier.MonitorID, barrier.AlertDelivery),
		CommandID:                    commandID,
		BarrierID:                    barrier.BarrierID,
		PreparationCommandID:         member.PreparationCommandID,
		Kind:                         kind,
		CreatedAt:                    created,
		ExpiresAt:                    expires,
	}
	if !domain.ValidDeliveryCutoverCommand(request) {
		return nil, domain.ProbeCommandMetadata{}, ports.ErrConflict
	}
	payload, err := r.codec.EncodeDeliveryCutover(ctx, request)
	if err != nil {
		return nil, domain.ProbeCommandMetadata{}, err
	}
	decoded, err := r.codec.DecodeDeliveryCutover(ctx, payload)
	if err != nil || decoded.CommandID != request.CommandID || decoded.Kind != kind ||
		decoded.ProbeDeliveryModeApplication != request.ProbeDeliveryModeApplication ||
		!decoded.CreatedAt.Equal(created) || !decoded.ExpiresAt.Equal(expires) {
		clear(payload)
		return nil, domain.ProbeCommandMetadata{}, domain.ErrValidation
	}
	generation := request.Generation
	meta := domain.ProbeCommandMetadata{
		CommandID:            commandID,
		HubID:                hub,
		ProbeID:              member.ProbeID,
		StreamID:             member.StreamID,
		Kind:                 kind,
		SourceAlertID:        nil,
		AssignmentGeneration: &generation,
		CreatedAt:            created,
		ExpiresAt:            expires,
		PayloadSHA256:        hex.EncodeToString(decoded.PayloadHash[:]),
	}
	if !domain.ValidProbeCommandMetadata(meta) || meta.CreatedAt.Nanosecond()%1000 != 0 || meta.ExpiresAt.Nanosecond()%1000 != 0 {
		clear(payload)
		return nil, domain.ProbeCommandMetadata{}, domain.ErrValidation
	}
	protected, err := r.protector.SealCommand(ctx, meta, payload)
	clear(payload)
	if err != nil {
		return nil, domain.ProbeCommandMetadata{}, err
	}
	return protected, meta, nil
}

// operativeCommandOutcomeTx copies the immutable source receipt for one issued
// command into the barrier's kind log and classifies it. The log is append-only:
// a contradictory duplicate result is a blocking conflict, never a rewrite.
func (r *OperativeDeliveryStore) operativeCommandOutcomeTx(ctx context.Context, tx bun.Tx, member *operativeMemberModel, kind string, _ time.Time) (operativeCommandState, error) {
	var commandID *string
	switch kind {
	case domain.DeliveryQuiesceKind:
		commandID = member.QuiesceCommandID
	case domain.DeliveryApplyKind:
		commandID = member.ApplyCommandID
	case domain.DeliveryResumeKind:
		commandID = member.ResumeCommandID
	}
	if commandID == nil {
		return operativeCommandState{}, ports.ErrConflict
	}
	var log operativeCommandModel
	if err := tx.NewSelect().Model(&log).Where("command_id = ?", *commandID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return operativeCommandState{}, ports.ErrConflict
		}
		return operativeCommandState{}, err
	}
	if log.BarrierID != member.BarrierID || log.ProbeID != member.ProbeID || log.Kind != kind || log.IssuedAt.IsZero() || !log.ExpiresAt.After(log.IssuedAt) {
		return operativeCommandState{}, ports.ErrConflict
	}
	if log.OutcomeStatus == "" {
		if member.ProbeID == domain.LocalProbeID {
			return operativeCommandState{}, ports.ErrConflict // Local effects commit with their receipt.
		}
		var command probeCommandRow
		err := tx.NewSelect().Model(&command).Where("command_id = ?", *commandID).Scan(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return operativeCommandState{}, ports.ErrConflict
			}
			return operativeCommandState{}, err
		}
		if command.Kind != kind || command.ProbeID != member.ProbeID || command.StreamID != member.StreamID ||
			command.SourceAlertID != nil || command.AssignmentGeneration == nil || *command.AssignmentGeneration != member.Generation {
			return operativeCommandState{}, ports.ErrConflict
		}
		if !command.RemoteConfirmed {
			return operativeCommandState{Pending: true}, nil
		}
		if !operativeCutoverOutcomeShape(kind, command) {
			return operativeCommandState{Blocked: true}, nil
		}
		log.OutcomeStatus = command.Status
		log.OutcomeCode = command.ResultCode
		at := command.ResultAppliedAt
		if at == nil {
			// A refusal or expiry receipt carries no applied time. The rearm
			// cooldown runs from the receipt itself (its commit clock), never from
			// the command's 24h expiry: a rejected-busy receipt must rearm after
			// the short cooldown, not a day later. The expiry stays the fallback
			// for a row without any update clock.
			stamp := command.UpdatedAt.UTC().Truncate(time.Microsecond)
			if command.UpdatedAt.IsZero() {
				stamp = command.ExpiresAt.UTC().Truncate(time.Microsecond)
			}
			at = &stamp
		}
		log.OutcomeAt = at
		if _, err := tx.NewUpdate().Model(&log).Column("outcome_status", "outcome_code", "outcome_at").WherePK().Exec(ctx); err != nil {
			return operativeCommandState{}, err
		}
	}
	if log.OutcomeAt == nil {
		return operativeCommandState{}, ports.ErrConflict
	}
	switch log.OutcomeStatus {
	case "applied", "already_applied", "already_resolved":
		if log.OutcomeCode != domain.DeliveryCutoverSuccessCode(kind) {
			return operativeCommandState{Blocked: true}, nil
		}
		return operativeCommandState{Success: true}, nil
	case "rejected":
		if log.OutcomeCode == operativeBusyCode {
			return operativeCommandState{Rearmable: true}, nil
		}
		return operativeCommandState{Blocked: true}, nil
	case "expired":
		return operativeCommandState{Rearmable: true}, nil
	default:
		return operativeCommandState{Blocked: true}, nil
	}
}

// validDeliveryCutoverResultShape validates one cutover result's bounded shape
// without knowing its kind. Success for these kinds always carries a
// non-empty kind-specific code; code-empty success is invalid.
func validDeliveryCutoverResultShape(r domain.ProbeCommandOutcome) bool {
	if r.CredentialVersion != 0 || r.CertificateVersion != 0 || r.CertificateFingerprint != "" || r.CertificateNotAfter != nil {
		return false
	}
	if !domain.ValidHubID(r.CommandID) || len(r.Message) > 4096 || len(r.Code) > 128 {
		return false
	}
	switch r.Status {
	case "applied", "already_applied", "already_resolved":
		return r.AppliedAt != nil && !r.AppliedAt.IsZero() && r.Code != ""
	case "rejected":
		return r.AppliedAt == nil && r.Code != ""
	case "expired":
		return r.AppliedAt == nil
	default:
		return false
	}
}

// validDeliveryCutoverResult binds one result to its kind-specific success
// code (delivery_quiesced/delivery_applied/delivery_resumed).
func validDeliveryCutoverResult(kind string, r domain.ProbeCommandOutcome) bool {
	if domain.DeliveryCutoverSuccessCode(kind) == "" || !validDeliveryCutoverResultShape(r) {
		return false
	}
	switch r.Status {
	case "applied", "already_applied", "already_resolved":
		return r.Code == domain.DeliveryCutoverSuccessCode(kind)
	case "rejected":
		return r.Code != domain.DeliveryCutoverSuccessCode(kind)
	default:
		return true
	}
}

// operativeCutoverOutcomeShape validates one cutover result shape from its
// stored command row. Success carries the kind-specific code and applied_at;
// a code-empty success for these kinds is invalid. Credential/certificate
// details are never present.
func operativeCutoverOutcomeShape(kind string, command probeCommandRow) bool {
	if command.ResultCredentialVersion != 0 || command.ResultCertificateVersion != 0 || command.ResultCertificateFingerprint != "" || command.ResultCertificateNotAfter != nil {
		return false
	}
	if len(command.ResultMessage) > 4096 || len(command.ResultCode) > 128 {
		return false
	}
	switch command.Status {
	case "applied", "already_applied", "already_resolved":
		return command.ResultAppliedAt != nil && !command.ResultAppliedAt.IsZero() && command.ResultCode == domain.DeliveryCutoverSuccessCode(kind)
	case "rejected":
		return command.ResultAppliedAt == nil && command.ResultCode != "" && command.ResultCode != domain.DeliveryCutoverSuccessCode(kind)
	case "expired":
		return command.ResultAppliedAt == nil
	default:
		return false
	}
}

// progressTx advances quiescing/quiesced/applying. Persisted holds stay
// enforced regardless of the opt-in; every pass rechecks current drift first so
// a stale/new configuration forces drain instead of a rebind.
func (r *OperativeDeliveryStore) progressTx(ctx context.Context, tx bun.Tx, state *operativeStateModel, hub string, evaluator ports.AggregateCutoverEvaluator, now time.Time) (bool, error) {
	if state.BarrierID == nil {
		return true, operativeBlockTx(state, now, "missing_barrier")
	}
	barrier, members, err := operativeBarrierTx(ctx, tx, *state.BarrierID)
	if err != nil {
		return false, err
	}
	if barrier.MonitorID != state.MonitorID || barrier.HubID != hub || barrier.AlertDelivery != state.AlertDelivery ||
		barrier.AssignmentRevision != state.AssignmentRevision {
		return true, operativeBlockTx(state, now, "barrier_identity_conflict")
	}
	drifted, corrupt, err := r.operativeDriftTx(ctx, tx, hub, barrier, members, state, now)
	if err != nil {
		return false, err
	}
	if corrupt {
		return true, operativeBlockTx(state, now, "barrier_evidence_conflict")
	}
	if drifted {
		return r.enterDrainingTx(state, now, "configuration_changed")
	}
	changed := false
	switch state.Phase {
	case operativePhaseQuiescing:
		kind := domain.DeliveryQuiesceKind
		for i := range members {
			done, again, blocked, err := r.memberCommandPassTx(ctx, tx, hub, barrier, &members[i], kind, now)
			if err != nil {
				return false, err
			}
			if blocked {
				return true, operativeBlockTx(state, now, "quiesce_refused")
			}
			if done && members[i].QuiescedAt == nil {
				at := now.UTC().Truncate(time.Microsecond)
				members[i].QuiescedAt = &at
				if _, err := tx.NewUpdate().Model(&members[i]).Column("quiesced_at").WherePK().Exec(ctx); err != nil {
					return false, err
				}
				changed = true
			}
			changed = changed || again
		}
		for i := range members {
			if members[i].QuiescedAt == nil {
				return changed, nil
			}
		}
		unresolved, err := operativeUnresolvedAttemptsTx(ctx, tx, state.MonitorID)
		if err != nil {
			return false, err
		}
		if unresolved {
			return changed, nil // Busy/ambiguous refusal is not a success.
		}
		state.Phase = operativePhaseQuiesced
		return true, nil
	case operativePhaseQuiesced:
		for i := range members {
			if members[i].ApplyCommandID != nil {
				continue
			}
			if _, err := r.issueCutoverCommandTx(ctx, tx, hub, barrier, &members[i], domain.DeliveryApplyKind, now); err != nil {
				return false, err
			}
		}
		state.Phase = operativePhaseApplying
		return true, nil
	case operativePhaseApplying:
		kind := domain.DeliveryApplyKind
		for i := range members {
			done, again, blocked, err := r.memberCommandPassTx(ctx, tx, hub, barrier, &members[i], kind, now)
			if err != nil {
				return false, err
			}
			if blocked {
				return true, operativeBlockTx(state, now, "apply_refused")
			}
			if done && members[i].AppliedAt == nil {
				at := now.UTC().Truncate(time.Microsecond)
				members[i].AppliedAt = &at
				if _, err := tx.NewUpdate().Model(&members[i]).Column("applied_at").WherePK().Exec(ctx); err != nil {
					return false, err
				}
				changed = true
			}
			changed = changed || again
		}
		for i := range members {
			if members[i].AppliedAt == nil {
				return changed, nil
			}
		}
		// ACTIVE only after every exact successful application receipt commits,
		// with the pure cutover evidence evaluated separately from authority.
		decision, err := r.operativeCutoverEvidenceTx(ctx, tx, hub, barrier, members, state, evaluator, now)
		if err != nil {
			return false, err
		}
		if decision == nil {
			return changed, nil
		}
		if decision.State != domain.DeliveryCutoverEvidenceComplete {
			return true, operativeBlockTx(state, now, "cutover_evidence_incomplete")
		}
		for _, source := range decision.Sources {
			if source.State != domain.DeliveryCutoverEvidenceComplete {
				return true, operativeBlockTx(state, now, "cutover_evidence_incomplete")
			}
		}
		at := now.UTC().Truncate(time.Microsecond)
		barrier.ActivatedAt = &at
		if _, err := tx.NewUpdate().Model(&barrier).Column("activated_at").WherePK().Exec(ctx); err != nil {
			return false, err
		}
		state.Phase = operativePhaseActive
		return true, nil
	default:
		return false, ports.ErrConflict
	}
}

// memberCommandPassTx reconciles one member's current command and rearms
// rejected-busy/expired work with a new immutable command ID after the cooldown.
func (r *OperativeDeliveryStore) memberCommandPassTx(ctx context.Context, tx bun.Tx, hub string, barrier operativeBarrierModel, member *operativeMemberModel, kind string, now time.Time) (done, changed, blocked bool, err error) {
	outcome, err := r.operativeCommandOutcomeTx(ctx, tx, member, kind, now)
	if err != nil {
		return false, false, false, err
	}
	switch {
	case outcome.Success:
		return true, false, false, nil
	case outcome.Blocked:
		return false, false, true, nil
	case outcome.Rearmable:
		var log operativeCommandModel
		var commandID *string
		switch kind {
		case domain.DeliveryQuiesceKind:
			commandID = member.QuiesceCommandID
		case domain.DeliveryApplyKind:
			commandID = member.ApplyCommandID
		default:
			commandID = member.ResumeCommandID
		}
		if err := tx.NewSelect().Model(&log).Where("command_id = ?", *commandID).Scan(ctx); err != nil {
			return false, false, false, err
		}
		if log.OutcomeAt == nil || now.Before(log.OutcomeAt.Add(operativeRearmCooldown)) {
			return false, false, false, nil
		}
		if _, err := r.issueCutoverCommandTx(ctx, tx, hub, barrier, member, kind, now); err != nil {
			return false, false, false, err
		}
		return false, true, false, nil
	}
	return false, false, false, nil
}

// operativeDriftTx rechecks the current set, generations, streams and exact
// retained configuration against the immutable barrier tuples. Any change stops
// aggregate admission and forces drain with no rebind.
func (r *OperativeDeliveryStore) operativeDriftTx(ctx context.Context, tx bun.Tx, hub string, barrier operativeBarrierModel, members []operativeMemberModel, state *operativeStateModel, _ time.Time) (drifted, corrupt bool, err error) {
	set, err := readProbeAssignments(ctx, tx, state.MonitorID)
	if errors.Is(err, ports.ErrNotFound) {
		return true, false, nil
	}
	if err != nil {
		return false, false, err
	}
	mode, ok := domain.CanonicalAlertDelivery(set.AlertDelivery)
	if !ok {
		return false, true, nil
	}
	if set.Revision != barrier.AssignmentRevision || mode != barrier.AlertDelivery || len(set.Assignments) != len(members) {
		return true, false, nil
	}
	for i, member := range members {
		assignment := set.Assignments[i]
		if assignment.ProbeID != member.ProbeID || assignment.Generation != member.Generation {
			return true, false, nil
		}
		ev, err := r.readOperativeMemberEvidenceTx(ctx, tx, hub, state.MonitorID, assignment, mode)
		if err != nil {
			return false, false, err
		}
		if ev.Evidence == operativeEvidenceCorrupt {
			return false, true, nil
		}
		if ev.Evidence != operativeEvidenceComplete {
			return true, false, nil
		}
		if ev.Assignment == nil || ev.Assignment.AssignmentRevision != member.AssignmentRevision || ev.Assignment.Generation != member.Generation ||
			ev.Assignment.AlertDelivery != mode || ev.ConfigRevision != member.ConfigRevision || ev.SHA256 != member.SHA256 || ev.StreamID != member.StreamID {
			return true, false, nil
		}
	}
	return false, false, nil
}

// operativeCutoverEvidenceTx assembles the pure cutover evidence from captured
// stored work: retained ACK/config/binding, current stream, stored regional
// facts and the committed aggregate/both application receipts. The evaluator is
// pure and confers no authority; nil means the evidence is not yet assembled.
func (r *OperativeDeliveryStore) operativeCutoverEvidenceTx(ctx context.Context, tx bun.Tx, hub string, barrier operativeBarrierModel, members []operativeMemberModel, state *operativeStateModel, evaluator ports.AggregateCutoverEvaluator, now time.Time) (*domain.DeliveryCutoverDecision, error) {
	set, err := readProbeAssignments(ctx, tx, state.MonitorID)
	if err != nil {
		return nil, err
	}
	mode := barrier.AlertDelivery
	in := domain.DeliveryCutoverInput{Now: now, SnapshotAt: now, Current: true, HubID: hub, Assignments: *set}
	for i := range members {
		if i >= len(set.Assignments) {
			return nil, ports.ErrConflict
		}
		assignment := set.Assignments[i]
		ev, err := r.readOperativeMemberEvidenceTx(ctx, tx, hub, state.MonitorID, assignment, mode)
		if err != nil {
			return nil, err
		}
		if ev.Evidence != operativeEvidenceComplete {
			return nil, nil
		}
		application, err := r.operativeApplicationTx(ctx, tx, hub, barrier.BarrierID, members[i], mode)
		if err != nil {
			return nil, err
		}
		if application == nil {
			return nil, nil
		}
		in.Sources = append(in.Sources, domain.DeliveryCutoverSource{
			ProbeID:                  members[i].ProbeID,
			StreamID:                 members[i].StreamID,
			ModeApplicationSupported: ev.Capable,
			AcknowledgedConfig:       ev.Acknowledged,
			Assignment:               ev.Assignment,
			ModeApplication:          application,
		})
	}
	decision, err := evaluator.EvaluateCutover(in)
	if err != nil {
		return nil, err
	}
	return &decision, nil
}

// activeTx keeps one active barrier honest: any drift or opt-in disable stops
// aggregate admission and drains. Genuine aggregate incident/intent admission
// reuses the pure ANY/ALL/UNKNOWN evaluator but is separately fenced.
func (r *OperativeDeliveryStore) activeTx(ctx context.Context, tx bun.Tx, state *operativeStateModel, hub string, enabled bool, evaluator ports.AggregateCutoverEvaluator, now time.Time) (bool, error) {
	if state.BarrierID == nil {
		return true, operativeBlockTx(state, now, "missing_barrier")
	}
	barrier, members, err := operativeBarrierTx(ctx, tx, *state.BarrierID)
	if err != nil {
		return false, err
	}
	if barrier.ActivatedAt == nil {
		return true, operativeBlockTx(state, now, "barrier_not_activated")
	}
	drifted, corrupt, err := r.operativeDriftTx(ctx, tx, hub, barrier, members, state, now)
	if err != nil {
		return false, err
	}
	if corrupt {
		return true, operativeBlockTx(state, now, "barrier_evidence_conflict")
	}
	if drifted {
		return r.enterDrainingTx(state, now, "configuration_changed")
	}
	if !enabled {
		return r.enterDrainingTx(state, now, "feature_disabled")
	}
	decision, err := r.operativeCutoverEvidenceTx(ctx, tx, hub, barrier, members, state, evaluator, now)
	if err != nil {
		return false, err
	}
	if decision == nil || decision.State != domain.DeliveryCutoverEvidenceComplete {
		return r.enterDrainingTx(state, now, "cutover_evidence_incomplete")
	}
	return r.aggregatePagingTx(ctx, tx, hub, barrier, members, state, evaluator, now)
}

func (r *OperativeDeliveryStore) enterDrainingTx(state *operativeStateModel, _ time.Time, reason string) (bool, error) {
	if len(reason) > 128 {
		reason = reason[:128]
	}
	if state.Phase == operativePhaseDraining {
		if state.Reason == "" {
			state.Reason = reason
		}
		return false, nil
	}
	state.Phase = operativePhaseDraining
	state.Reason = reason
	return true, nil
}

// drainingTx stops new aggregate admissions, supersedes unstarted plans, and
// releases each ORIGINAL member gate only after every admitted provider attempt
// is resolved. In-flight/ambiguous grants block resume issuance forever until
// an explicit operator recovery; nothing here fabricates completion. Removed or
// offline members are never dropped from drain membership.
func (r *OperativeDeliveryStore) drainingTx(ctx context.Context, tx bun.Tx, state *operativeStateModel, hub string, now time.Time) (bool, error) {
	if state.BarrierID == nil {
		return true, operativeBlockTx(state, now, "missing_barrier")
	}
	barrier, members, err := operativeBarrierTx(ctx, tx, *state.BarrierID)
	if err != nil {
		return false, err
	}
	changed, err := r.supersedeAggregatePlansTx(ctx, tx, state.MonitorID, barrier.BarrierID, now)
	if err != nil {
		return false, err
	}
	unresolved, err := operativeUnresolvedAttemptsTx(ctx, tx, state.MonitorID)
	if err != nil {
		return false, err
	}
	if unresolved {
		return changed, nil // No source resumes regional until these resolve.
	}
	for i := range members {
		if members[i].ResumeCommandID == nil {
			// Old queued phase commands can never re-hold after resume: withdraw
			// this member's still-queued unconfirmed phase requests first.
			if err := withdrawQueuedPhaseCommandsTx(ctx, tx, &members[i], now); err != nil {
				return false, err
			}
			if _, err := r.issueCutoverCommandTx(ctx, tx, hub, barrier, &members[i], domain.DeliveryResumeKind, now); err != nil {
				return false, err
			}
			changed = true
			continue
		}
		done, again, blocked, err := r.memberCommandPassTx(ctx, tx, hub, barrier, &members[i], domain.DeliveryResumeKind, now)
		if err != nil {
			return false, err
		}
		if blocked {
			return true, operativeBlockTx(state, now, "resume_refused")
		}
		if done && members[i].ResumedAt == nil {
			at := now.UTC().Truncate(time.Microsecond)
			members[i].ResumedAt = &at
			if _, err := tx.NewUpdate().Model(&members[i]).Column("resumed_at").WherePK().Exec(ctx); err != nil {
				return false, err
			}
			changed = true
		}
		changed = changed || again
	}
	for i := range members {
		if members[i].ResumedAt == nil {
			return changed, nil
		}
	}
	// Resume authenticates the stored barrier tuple and releases only this
	// barrier's gate; it never releases a replacement barrier's hold.
	if _, err := tx.NewUpdate().Model((*operativeGateModel)(nil)).Set("state = ?", "released").
		Set("released_at = ?", now.UTC().Truncate(time.Microsecond)).
		Where("barrier_id = ? AND state = ?", barrier.BarrierID, "held").Exec(ctx); err != nil {
		return false, err
	}
	state.Phase = operativePhaseReleased
	state.Reason = ""
	return true, nil
}

// withdrawQueuedPhaseCommandsTx cancels one member's still-queued unconfirmed
// quiesce/apply requests at resume issuance. A canceled request is no longer
// claimable and can never carry a receipt, so an old phase command can never
// re-hold a source after its resume. Confirmed receipts stay immutable history,
// and the source-side release tombstone remains the probe's own contract.
func withdrawQueuedPhaseCommandsTx(ctx context.Context, tx bun.Tx, member *operativeMemberModel, now time.Time) error {
	ids := make([]string, 0, 2)
	if member.QuiesceCommandID != nil {
		ids = append(ids, *member.QuiesceCommandID)
	}
	if member.ApplyCommandID != nil {
		ids = append(ids, *member.ApplyCommandID)
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := tx.NewUpdate().Model((*probeCommandRow)(nil)).
		Set("status = ?", "canceled").
		Set("next_attempt_at = NULL").
		Set("result_code = ?", "cutover_phase_withdrawn").
		Set("result_message = ?", "Barrier drain resume supersedes the queued phase command").
		Set("updated_at = ?", now.UTC().Truncate(time.Microsecond)).
		Where("command_id IN (?) AND remote_confirmed = ? AND status IN (?, ?)", bun.List(ids), false, "pending", "blocked").
		Exec(ctx)
	return err
}
