package repository

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeStateReceiptModel struct {
	bun.BaseModel  `bun:"table:probe_state_receipts"`
	ProbeID        string `bun:",pk"`
	StreamID       string `bun:",pk"`
	SnapshotID     string
	SHA256         string `bun:"sha256"`
	ConfigRevision int64
	LastCreatedSeq int64
	CreatedAt      time.Time
	AppliedAt      time.Time
	StateCount     int
}

func (r probeStateReceiptModel) receipt() *domain.ProbeStateReceipt {
	return &domain.ProbeStateReceipt{SnapshotID: r.SnapshotID, StreamID: r.StreamID, ConfigRevision: r.ConfigRevision, SHA256: r.SHA256, AppliedAt: r.AppliedAt.UTC(), StateCount: r.StateCount}
}

type probeMissingStateModel struct {
	bun.BaseModel        `bun:"table:probe_missing_state,alias:missing"`
	MonitorID            int64  `bun:",pk"`
	ProbeID              string `bun:",pk"`
	StreamID             string
	AssignmentGeneration int64
	ConfigRevision       int64
	SnapshotSeq          int64
	Reason               string
	CreatedAt            time.Time
	AppliedAt            time.Time
}

var _ ports.ProbeStateRepository = (*ProbeReplayStore)(nil)

// ApplyCurrentSnapshot atomically applies complete current evidence and its
// receipt. It never inserts history, advances a cursor or creates provider work.
func (s *ProbeReplayStore) ApplyCurrentSnapshot(ctx context.Context, session domain.ProbeReplaySession, snapshot domain.ProbeCurrentSnapshot, authorizer ports.ProbeStateAuthorizer) (*domain.ProbeStateReceipt, error) {
	if s == nil || s.db == nil || s.protector == nil || s.decoder == nil || authorizer == nil || !domain.ValidProbeCurrentSnapshot(session, snapshot) {
		return nil, domain.ErrValidation
	}
	var result *domain.ProbeStateReceipt
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		authority, err := s.lockReplaySession(ctx, tx, session)
		if err != nil {
			return err
		}
		var prior probeStateReceiptModel
		err = tx.NewSelect().Model(&prior).Where("probe_id = ? AND stream_id = ?", session.ProbeID, session.StreamID).Scan(ctx)
		exists := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if exists {
			if prior.SnapshotID == snapshot.SnapshotID {
				if prior.SHA256 != snapshot.SHA256 || prior.ConfigRevision != snapshot.ConfigRevision || prior.LastCreatedSeq != snapshot.LastCreatedSeq || prior.StateCount != len(snapshot.States) {
					return ports.ErrConflict
				}
				result = prior.receipt()
				return nil
			}
			if snapshot.ConfigRevision < prior.ConfigRevision || snapshot.LastCreatedSeq < prior.LastCreatedSeq {
				return ports.ErrConflict
			}
		}
		assignments, err := s.currentSnapshotAssignments(ctx, tx, session, snapshot.ConfigRevision)
		if err != nil {
			return err
		}
		facts := domain.ProbeStateAuthorityFacts{ProbeID: session.ProbeID, StreamID: session.StreamID, ConfigRevision: snapshot.ConfigRevision, Assignments: assignments}
		if !authorizer.AuthorizeCurrentSnapshot(ctx, facts, snapshot) {
			return ports.ErrConflict
		}
		byMonitor := make(map[int64]domain.ProbeCurrentState, len(snapshot.States))
		for _, state := range snapshot.States {
			if state.ActiveSourceAlertID != nil {
				var incident probeIncidentModel
				err := tx.NewSelect().Model(&incident).Where("source_alert_id = ?", *state.ActiveSourceAlertID).Scan(ctx)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if err == nil && (incident.ProbeID != session.ProbeID || incident.MonitorID == nil || *incident.MonitorID != state.MonitorID || incident.AssignmentGeneration == nil || *incident.AssignmentGeneration != state.AssignmentGeneration || incident.SubjectKind != domain.IncidentSubjectAvailability) {
					return ports.ErrConflict
				}
			}
			byMonitor[state.MonitorID] = state
		}
		slices.SortFunc(assignments, func(a, b domain.EdgeAssignmentIdentity) int {
			if a.MonitorID < b.MonitorID {
				return -1
			}
			if a.MonitorID > b.MonitorID {
				return 1
			}
			return 0
		})
		for _, assignment := range assignments {
			if !assignment.Active {
				continue
			}
			// Keep assignment replacement and snapshot application serializable, using
			// the same sorted monitor lock order as other configuration authority writes.
			if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = revision WHERE monitor_id = ?", assignment.MonitorID); err != nil {
				return err
			}
			if err := requireActiveAssignment(ctx, tx, assignment.MonitorID, session.ProbeID, assignment.Generation); err != nil {
				if errors.Is(err, ports.ErrNotFound) || errors.Is(err, ports.ErrConflict) {
					continue
				}
				return err
			}
			if state, ok := byMonitor[assignment.MonitorID]; ok {
				candidate := domain.RegionalState{MonitorID: state.MonitorID, ProbeID: session.ProbeID, AssignmentGeneration: state.AssignmentGeneration, StreamID: session.StreamID, Seq: state.Seq, ConfigRevision: snapshot.ConfigRevision, Status: state.Status, DownCount: state.DownCount, Ping: state.Ping, Message: state.Message, ActiveSourceAlertID: state.ActiveSourceAlertID, ObservedAt: state.ObservedAt.UTC(), ReceivedAt: authority.now}
				if err := updateCurrentProbeState(ctx, tx, candidate, authority.now, true); err != nil {
					return err
				}
			} else if err := applyMissingState(ctx, tx, session, snapshot, assignment, authority.now); err != nil {
				return err
			}
		}
		row := probeStateReceiptModel{ProbeID: session.ProbeID, StreamID: session.StreamID, SnapshotID: snapshot.SnapshotID, SHA256: snapshot.SHA256, ConfigRevision: snapshot.ConfigRevision, LastCreatedSeq: snapshot.LastCreatedSeq, CreatedAt: snapshot.CreatedAt.UTC(), AppliedAt: authority.now, StateCount: len(snapshot.States)}
		if exists {
			_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		} else {
			_, err = tx.NewInsert().Model(&row).Exec(ctx)
		}
		if err != nil {
			return err
		}
		committedAt, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if authority.lease.LeaseUntil <= committedAt.Unix() {
			return ports.ErrConflict
		}
		result = row.receipt()
		return nil
	})
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	return result, nil
}

func (s *ProbeReplayStore) currentSnapshotAssignments(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, revision int64) ([]domain.EdgeAssignmentIdentity, error) {
	var active probeActiveConfigModel
	if err := tx.NewSelect().Model(&active).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
		return nil, err
	}
	if active.Revision != revision || active.HubID != session.HubID {
		return nil, ports.ErrConflict
	}
	var snapshot probeConfigModel
	if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", session.ProbeID, revision).Scan(ctx); err != nil {
		return nil, err
	}
	if snapshot.HubID != session.HubID || snapshot.SHA256 != active.SHA256 {
		return nil, ports.ErrConflict
	}
	plain, err := s.protector.Open(ctx, snapshot.domain().ProbeConfigMetadata, snapshot.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	resolved, err := s.decoder.DecodeEdge(ctx, plain, domain.ProbeConfigTarget{HubID: session.HubID, ProbeID: session.ProbeID})
	if err != nil {
		return nil, err
	}
	out := make([]domain.EdgeAssignmentIdentity, 0, len(resolved.Assignments))
	for _, a := range resolved.Assignments {
		if a.Monitor == nil {
			return nil, domain.ErrInternal
		}
		out = append(out, domain.EdgeAssignmentIdentity{MonitorID: a.Monitor.ID, Generation: a.Generation, Active: a.Monitor.Active})
	}
	return out, nil
}

func applyMissingState(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, snapshot domain.ProbeCurrentSnapshot, assignment domain.EdgeAssignmentIdentity, now time.Time) error {
	var current monitorProbeStateModel
	err := tx.NewSelect().Model(&current).Where("monitor_id = ? AND probe_id = ?", assignment.MonitorID, session.ProbeID).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && (current.AssignmentGeneration > assignment.Generation || current.StreamID == session.StreamID && current.Seq > snapshot.LastCreatedSeq) {
		return nil
	}
	var previous probeMissingStateModel
	err = tx.NewSelect().Model(&previous).Where("monitor_id = ? AND probe_id = ?", assignment.MonitorID, session.ProbeID).Scan(ctx)
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if exists && (previous.AssignmentGeneration > assignment.Generation || previous.StreamID == session.StreamID && previous.SnapshotSeq > snapshot.LastCreatedSeq) {
		return nil
	}
	if _, err := tx.NewDelete().Model((*monitorProbeStateModel)(nil)).Where("monitor_id = ? AND probe_id = ?", assignment.MonitorID, session.ProbeID).Exec(ctx); err != nil {
		return err
	}
	row := probeMissingStateModel{MonitorID: assignment.MonitorID, ProbeID: session.ProbeID, StreamID: session.StreamID, AssignmentGeneration: assignment.Generation, ConfigRevision: snapshot.ConfigRevision, SnapshotSeq: snapshot.LastCreatedSeq, CreatedAt: snapshot.CreatedAt.UTC(), AppliedAt: now, Reason: "missing_snapshot_state"}
	if exists {
		_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
	} else {
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
	}
	return err
}

// readCurrentStates keeps the one-query read budget of the existing repository
// while exposing omission markers without inventing persisted observations.
func (r *RegionalCommitStore) readCurrentStates(ctx context.Context, monitorID int64, probeID string) ([]domain.RegionalState, error) {
	filter := ""
	args := []any{monitorID}
	if probeID != "" {
		filter = " AND probe_id = ?"
		args = append(args, probeID)
	}
	args = append(args, args...)
	query := `SELECT monitor_id,probe_id,assignment_generation,stream_id,seq,config_revision,status,down_count,observed_at,received_at,last_success_at,ping,message,active_source_alert_id,'' AS unknown_reason
 FROM monitor_probe_state WHERE monitor_id = ?` + filter + `
 UNION ALL SELECT monitor_id,probe_id,assignment_generation,stream_id,snapshot_seq,config_revision,4,0,created_at,applied_at,NULL,0,'',NULL,reason
 FROM probe_missing_state AS missing WHERE monitor_id = ?` + filter + `
 AND NOT EXISTS (SELECT 1 FROM monitor_probe_state AS current WHERE current.monitor_id = missing.monitor_id AND current.probe_id = missing.probe_id)
 ORDER BY probe_id`
	var rows []struct {
		monitorProbeStateModel `bun:",extend"`
		UnknownReason          string
	}
	if err := r.db.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, probeRegistryError(err)
	}
	out := make([]domain.RegionalState, 0, len(rows))
	for _, row := range rows {
		state := row.state()
		state.UnknownReason = row.UnknownReason
		out = append(out, state)
	}
	return out, nil
}
