package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// operativeEvidenceState classifies one member's captured evidence. Missing
// evidence is pending; only internal incoherence or contradiction is corrupt.
type operativeEvidenceState int

const (
	operativeEvidenceComplete operativeEvidenceState = iota
	operativeEvidencePending
	operativeEvidenceCorrupt
)

// operativeMemberEvidence is one member's exact captured source evidence. All
// fields come from authenticated retained state (document/receipt/active
// pointer/region facts), never mutable-source reconstruction.
type operativeMemberEvidence struct {
	Evidence           operativeEvidenceState
	ProbeID            string
	Generation         int64
	AssignmentRevision int64
	ConfigRevision     int64
	SHA256             string
	StreamID           string
	Mode               domain.AlertDelivery
	Capable            bool
	Acknowledged       *domain.ProbeActiveConfig
	Assignment         *domain.AcknowledgedAssignmentDelivery
	RegionalFact       *domain.ProbeDeliveryModeApplication
	LocalGraph         *domain.LocalProbeConfigDefinition
}

func (e operativeMemberEvidence) tuple(monitorID int64) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{ProbeID: e.ProbeID},
		MonitorID:          monitorID,
		AssignmentRevision: e.AssignmentRevision,
		Generation:         e.Generation,
		ConfigRevision:     e.ConfigRevision,
		SHA256:             e.SHA256,
		StreamID:           e.StreamID,
		AlertDelivery:      e.Mode,
	}
}

// readOperativeMemberEvidenceTx independently proves one member's selected
// protected configuration, ACK, set binding, current stream and stored regional
// fact. Remote documents are verified only through the retained protected
// document/receipt machinery.
func (r *OperativeDeliveryStore) readOperativeMemberEvidenceTx(ctx context.Context, tx bun.Tx, hub string, monitorID int64, assignment domain.ProbeAssignment, mode domain.AlertDelivery) (operativeMemberEvidence, error) {
	out := operativeMemberEvidence{Evidence: operativeEvidencePending, ProbeID: assignment.ProbeID, Generation: assignment.Generation, AssignmentRevision: 0, Mode: mode}
	if assignment.ProbeID == domain.LocalProbeID {
		return r.localMemberEvidenceTx(ctx, tx, hub, monitorID, assignment, mode)
	}
	// Remote member: committed active pointer, durable ACK and the retained
	// protected document decoded through the shared decoder.
	document := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: hub, ProbeID: assignment.ProbeID},
		MonitorID:          monitorID,
		AssignmentRevision: 0,
		Generation:         assignment.Generation,
		AlertDelivery:      mode,
	}
	var active probeActiveConfigModel
	if err := tx.NewSelect().Model(&active).Where("probe_id = ?", assignment.ProbeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, err
	}
	document.ConfigRevision = active.Revision
	document.SHA256 = active.SHA256
	session := domain.ProbeReplaySession{HubID: hub, ProbeID: assignment.ProbeID}
	members, stale, err := r.sync.preparationDocument(ctx, tx, session, document)
	if err != nil {
		if errors.Is(err, ports.ErrConflict) {
			out.Evidence = operativeEvidenceCorrupt
			return out, nil
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
			return out, nil
		}
		return out, err
	}
	if stale {
		return out, nil
	}
	resolved, ok := members[monitorID]
	if !ok || resolved.Monitor == nil {
		return out, nil
	}
	stream, accepted, err := operativeAcceptedStreamTx(ctx, tx, assignment.ProbeID)
	if err != nil {
		return out, err
	}
	if !accepted {
		return out, nil
	}
	if !domain.ValidProbeActiveConfig(domain.ProbeActiveConfig{ProbeConfigTarget: document.ProbeConfigTarget, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount}) {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	var receipt probeConfigAppliedReceiptModel
	if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", assignment.ProbeID, active.Revision).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			out.Evidence = operativeEvidenceCorrupt
			return out, nil
		}
		return out, err
	}
	if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	var fact remoteRegionalApplicationModel
	err = tx.NewSelect().Model(&fact).Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND stream_id = ?", assignment.ProbeID, active.Revision, monitorID, stream).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, err
	}
	if fact.AssignmentRevision != resolved.AssignmentRevision || fact.Generation != resolved.Generation || fact.HubID != hub ||
		fact.SHA256 != active.SHA256 || fact.AlertDelivery != domain.AlertDeliveryRegional || fact.ConnectionGeneration <= 0 || fact.RecordedAt.IsZero() {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	var capability operativeCapabilityModel
	err = tx.NewSelect().Model(&capability).Where("probe_id = ? AND capability = ?", assignment.ProbeID, domain.DeliveryCutoverCapability).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	app := domain.ProbeDeliveryModeApplication{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: fact.HubID, ProbeID: fact.ProbeID},
		MonitorID: fact.MonitorID, AssignmentRevision: fact.AssignmentRevision, Generation: fact.Generation,
		ConfigRevision: fact.ConfigRevision, SHA256: fact.SHA256, StreamID: fact.StreamID, AlertDelivery: fact.AlertDelivery}
	out.Evidence = operativeEvidenceComplete
	out.AssignmentRevision = resolved.AssignmentRevision
	out.ConfigRevision = active.Revision
	out.SHA256 = active.SHA256
	out.StreamID = stream
	out.Capable = false
	if err == nil {
		lease, err := readProbeSession(ctx, tx, assignment.ProbeID)
		if err != nil {
			return out, err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return out, err
		}
		out.Capable = capability.ConnectionGeneration == lease.Generation && lease.LeaseUntil > now.Unix()
	}
	out.Acknowledged = &domain.ProbeActiveConfig{ProbeConfigTarget: document.ProbeConfigTarget, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount}
	out.Assignment = &domain.AcknowledgedAssignmentDelivery{MonitorID: monitorID, Generation: resolved.Generation, AssignmentRevision: resolved.AssignmentRevision, AlertDelivery: resolved.DesiredAlertDelivery}
	out.RegionalFact = &app
	out.LocalGraph = nil
	return out, nil
}

// localMemberEvidenceTx proves the exact whole-source graph (re-encoded hash),
// the committed active pointer and ACK, and the stored source-local regional
// fact for this monitor.
func (r *OperativeDeliveryStore) localMemberEvidenceTx(ctx context.Context, tx bun.Tx, hub string, monitorID int64, assignment domain.ProbeAssignment, mode domain.AlertDelivery) (operativeMemberEvidence, error) {
	out := operativeMemberEvidence{Evidence: operativeEvidencePending, ProbeID: domain.LocalProbeID, Generation: assignment.Generation, Mode: mode, StreamID: domain.LocalStreamID, Capable: true}
	definition, err := readAppliedLocalTx(ctx, tx, r.encoder)
	if err != nil {
		if errors.Is(err, ports.ErrConflict) {
			out.Evidence = operativeEvidenceCorrupt
			return out, nil
		}
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ports.ErrNotFound) {
			return out, nil
		}
		return out, err
	}
	var active probeActiveConfigModel
	if err := tx.NewSelect().Model(&active).Where("probe_id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, err
	}
	var receipt probeConfigAppliedReceiptModel
	if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", domain.LocalProbeID, active.Revision).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			out.Evidence = operativeEvidenceCorrupt
			return out, nil
		}
		return out, err
	}
	if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	if active.HubID != hub {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	var member *domain.ProbeConfigAssignment
	for i := range definition.Assignments {
		if definition.Assignments[i].Monitor != nil && definition.Assignments[i].Monitor.ID == monitorID {
			member = &definition.Assignments[i]
			break
		}
	}
	if member == nil {
		return out, nil
	}
	fact, ok := localFactForMonitor(definition, monitorID)
	if !ok {
		return out, nil
	}
	if fact.AlertDelivery != domain.AlertDeliveryRegional {
		out.Evidence = operativeEvidenceCorrupt
		return out, nil
	}
	out.Evidence = operativeEvidenceComplete
	out.AssignmentRevision = member.AssignmentRevision
	out.ConfigRevision = active.Revision
	out.SHA256 = active.SHA256
	out.Acknowledged = &domain.ProbeActiveConfig{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: hub, ProbeID: domain.LocalProbeID}, Revision: active.Revision, SHA256: active.SHA256, AppliedAt: active.AppliedAt, AssignmentCount: active.AssignmentCount}
	out.Assignment = &domain.AcknowledgedAssignmentDelivery{MonitorID: monitorID, Generation: member.Generation, AssignmentRevision: member.AssignmentRevision, AlertDelivery: member.AlertDelivery}
	out.RegionalFact = &fact
	out.LocalGraph = definition
	return out, nil
}

func localFactForMonitor(definition *domain.LocalProbeConfigDefinition, monitorID int64) (domain.ProbeDeliveryModeApplication, bool) {
	if definition == nil {
		return domain.ProbeDeliveryModeApplication{}, false
	}
	for _, fact := range definition.RegionalDeliveryApplications {
		if fact.MonitorID == monitorID {
			return fact, true
		}
	}
	return domain.ProbeDeliveryModeApplication{}, false
}

// operativeAcceptedStreamTx returns the member's accepted current stream. A
// paused/disabled/offline member keeps its accepted stream; only retirement or
// a prepared reset invalidates it. No connected-only shrink is ever derived
// from this.
func operativeAcceptedStreamTx(ctx context.Context, tx bun.Tx, probeID string) (string, bool, error) {
	var stream string
	err := tx.NewRaw(`SELECT c.stream_id FROM probes p JOIN probe_connections c ON c.probe_id=p.id
 JOIN probe_streams s ON s.probe_id=p.id AND s.stream_id=c.stream_id
 WHERE p.id=? AND p.revoked_at IS NULL AND p.deleted_at IS NULL
 AND s.retired_at IS NULL
 AND NOT EXISTS(SELECT 1 FROM probe_stream_resets r WHERE r.probe_id=p.id AND r.state='prepared')`, probeID).Scan(ctx, &stream)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return stream, stream != "", nil
}

// operativePreparationRoundTx reads (never creates) the current shared
// preparation round for the exact desired set. Round creation belongs to the
// preparation machinery; the coordinator only binds to it.
func operativePreparationRoundTx(ctx context.Context, tx bun.Tx, hub string, monitor, revision int64, mode domain.AlertDelivery) (preparationRoundModel, bool, error) {
	var latest preparationRoundModel
	err := tx.NewSelect().Model(&latest).Where("monitor_id = ?", monitor).Order("request_round DESC").Limit(1).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return preparationRoundModel{}, false, nil
		}
		return preparationRoundModel{}, false, err
	}
	var members []preparationMemberModel
	if err := tx.NewSelect().Model(&members).Where("monitor_id = ? AND request_round = ?", monitor, latest.RequestRound).Order("probe_id ASC").Scan(ctx); err != nil {
		return preparationRoundModel{}, false, err
	}
	if !domain.ValidDeliveryPreparationRound(latest.round(members)) || latest.HubID != hub {
		return preparationRoundModel{}, false, ports.ErrConflict
	}
	set, err := readProbeAssignments(ctx, tx, monitor)
	if err != nil {
		return preparationRoundModel{}, false, err
	}
	if set.Revision != revision || set.AlertDelivery != mode {
		return preparationRoundModel{}, false, nil
	}
	if latest.AssignmentRevision != revision || latest.AlertDelivery != mode || len(members) != len(set.Assignments) {
		return preparationRoundModel{}, false, nil
	}
	for i, m := range members {
		if m.ProbeID != set.Assignments[i].ProbeID || m.Generation != set.Assignments[i].Generation {
			return preparationRoundModel{}, false, nil
		}
	}
	return latest, true, nil
}

// operativeMemberPreparedTx proves the exact shared nonce-bound preparation for
// one member: the round-bound request row, and for remote members the immutable
// successful delivery.prepare receipt bound to that request identity. Missing
// is pending; contradiction is corrupt.
func operativeMemberPreparedTx(ctx context.Context, tx bun.Tx, member operativeMemberEvidence, round preparationRoundModel) (string, operativeEvidenceState, error) {
	bound := member.tuple(round.MonitorID)
	bound.ProbeConfigTarget.HubID = round.HubID
	requestID := preparationRoundRequestID(bound, round)
	var request preparationRequestModel
	err := tx.NewSelect().Model(&request).Where("request_id = ?", requestID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", operativeEvidencePending, nil
		}
		return "", operativeEvidencePending, err
	}
	if request.MonitorID != round.MonitorID || request.RequestRound != round.RequestRound || request.ProbeID != bound.ProbeID ||
		request.Generation != bound.Generation || request.ConfigRevision != bound.ConfigRevision || request.StreamID != bound.StreamID ||
		request.SHA256 != bound.SHA256 || request.PreparedAt.IsZero() {
		return "", operativeEvidenceCorrupt, nil
	}
	if bound.ProbeID == domain.LocalProbeID {
		// The local pending request is written only on fresh exact whole-graph
		// activation; there is no separate local preparation receipt to prove.
		if request.CommandID != nil {
			return "", operativeEvidenceCorrupt, nil
		}
		return requestID, operativeEvidenceComplete, nil
	}
	if request.CommandID == nil || *request.CommandID != requestID {
		return "", operativeEvidenceCorrupt, nil
	}
	var command probeCommandRow
	err = tx.NewSelect().Model(&command).Where("command_id = ?", requestID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", operativeEvidenceCorrupt, nil
		}
		return "", operativeEvidencePending, err
	}
	if command.Kind != deliveryPrepareKind || command.ProbeID != bound.ProbeID || command.StreamID != bound.StreamID ||
		command.SourceAlertID != nil || command.AssignmentGeneration == nil || *command.AssignmentGeneration != bound.Generation {
		return "", operativeEvidenceCorrupt, nil
	}
	if !command.RemoteConfirmed || command.Status != "applied" && command.Status != "already_applied" {
		return "", operativeEvidencePending, nil
	}
	if command.ResultCode != deliveryPreparedResultCode || command.ResultAppliedAt == nil {
		return "", operativeEvidenceCorrupt, nil
	}
	return requestID, operativeEvidenceComplete, nil
}

// operativeApplicationTx returns the committed aggregate/both effective
// application evidence for one member: the source-local application row for the
// hub-local member, or the immutable successful apply receipt for a remote one.
func (r *OperativeDeliveryStore) operativeApplicationTx(ctx context.Context, tx bun.Tx, hub, barrierID string, member operativeMemberModel, mode domain.AlertDelivery) (*domain.ProbeDeliveryModeApplication, error) {
	bound := domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: hub, ProbeID: member.ProbeID},
		MonitorID:          member.MonitorID,
		AssignmentRevision: member.AssignmentRevision,
		Generation:         member.Generation,
		ConfigRevision:     member.ConfigRevision,
		SHA256:             member.SHA256,
		StreamID:           member.StreamID,
		AlertDelivery:      mode,
	}
	if member.ProbeID == domain.LocalProbeID {
		var row operativeLocalApplicationModel
		err := tx.NewSelect().Model(&row).Where("probe_id = ? AND config_revision = ? AND monitor_id = ? AND barrier_id = ?", domain.LocalProbeID, member.ConfigRevision, member.MonitorID, barrierID).Scan(ctx)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		if row.BarrierID != barrierID || row.AssignmentRevision != member.AssignmentRevision || row.Generation != member.Generation ||
			row.SHA256 != member.SHA256 || row.StreamID != member.StreamID || row.AlertDelivery != mode || row.AppliedAt.IsZero() {
			return nil, ports.ErrConflict
		}
		return &bound, nil
	}
	if member.ApplyCommandID == nil {
		return nil, nil
	}
	var log operativeCommandModel
	err := tx.NewSelect().Model(&log).Where("command_id = ?", *member.ApplyCommandID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrConflict
		}
		return nil, err
	}
	if log.BarrierID != barrierID || log.ProbeID != member.ProbeID || log.Kind != domain.DeliveryApplyKind {
		return nil, ports.ErrConflict
	}
	if log.OutcomeStatus != "applied" && log.OutcomeStatus != "already_applied" {
		return nil, nil
	}
	if log.OutcomeCode != domain.DeliveryAppliedCode || log.OutcomeAt == nil {
		return nil, ports.ErrConflict
	}
	return &bound, nil
}

// operativeUnresolvedAttemptsTx reports whether any admitted provider grant for
// this monitor is still in flight or ambiguous. Ambiguity is an I/O fact and is
// never cleared by timers, lease expiry, cancellation or restart.
func operativeUnresolvedAttemptsTx(ctx context.Context, tx bun.Tx, monitorID int64) (bool, error) {
	count, err := tx.NewSelect().Table("operative_provider_attempts").ColumnExpr("COUNT(*)").
		Where("monitor_id = ?", monitorID).Where("state IN (?, ?)", domain.ProviderAttemptInFlight, domain.ProviderAttemptAmbiguous).Count(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// operativeBlockTx persists a fail-closed phase without altering source holds.
func operativeBlockTx(state *operativeStateModel, now time.Time, reason string) error {
	if len(reason) > 128 {
		reason = reason[:128]
	}
	if state.TransitionVersion == math.MaxInt64 {
		return ports.ErrConflict
	}
	state.TransitionVersion++
	state.Phase = operativePhaseBlocked
	state.Reason = reason
	state.UpdatedAt = now
	return nil
}
