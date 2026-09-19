package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.LocalHeartbeatRecorder = (*RegionalCommitStore)(nil)

// CommitLocalHeartbeat records one local check with a durable global sequence.
// The first statement takes the allocator's write lock, including on SQLite,
// before any reads. The lock lasts through all writes and their commit.
func (r *RegionalCommitStore) CommitLocalHeartbeat(ctx context.Context, commit domain.LocalHeartbeatCommit) (*domain.Heartbeat, error) {
	callerIncident := commit.Incident
	commit.Incident = copyCommitIncident(commit.Incident)
	callerAlert := commit.Alert
	commit.Alert = copyCommitAlert(commit.Alert)
	callerEscalation := commit.Escalation
	commit.Escalation = copyCommitEscalation(commit.Escalation)
	hb := commit.Heartbeat
	if hb.ID != 0 || hb.SourceSeq != 0 || hb.MonitorID <= 0 || hb.ProbeID != domain.LocalProbeID ||
		hb.StreamID != domain.LocalStreamID || hb.AssignmentGeneration < 1 || commit.ExpectedStateSeq < 0 ||
		hb.Time.IsZero() || hb.ReceivedAt.IsZero() || hb.ConfigRevision < 1 ||
		hb.Status < domain.StatusDown || hb.Status > domain.StatusMaintenance ||
		commit.RawStatus < domain.StatusDown || commit.RawStatus > domain.StatusMaintenance ||
		hb.DownCount < 0 || hb.Ping < 0 || hb.Duration < 0 {
		return nil, fmt.Errorf("local heartbeat identity: %w", domain.ErrValidation)
	}
	hb.Time, hb.ReceivedAt = hb.Time.UTC(), hb.ReceivedAt.UTC()
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		result, err := tx.NewUpdate().TableExpr("probe_local_sequence").
			Set("last_seq = last_seq + 1").Where("id = 1 AND last_seq < ?", int64(math.MaxInt64)).Exec(ctx)
		if err != nil {
			return fmt.Errorf("allocate local sequence: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("local sequence missing or exhausted: %w", ports.ErrConflict)
		}
		if err := tx.NewSelect().TableExpr("probe_local_sequence").Column("last_seq").Where("id = 1").Scan(ctx, &hb.SourceSeq); err != nil {
			return err
		}

		// Lock the set before the member, matching assignment replacement order.
		// SQLite already holds the database writer lock from allocation above.
		var revision int64
		assignment := tx.NewSelect().TableExpr("monitor_probe_assignment_sets").Column("revision").Where("monitor_id = ?", hb.MonitorID)
		if tx.Dialect().Name() == dialect.MySQL {
			assignment = assignment.For("UPDATE")
		}
		if err := assignment.Scan(ctx, &revision); err != nil {
			return fmt.Errorf("lock local assignment: %w", probeRegistryError(err))
		}
		var activeGeneration int64
		member := tx.NewSelect().TableExpr("monitor_probe_assignments").Column("generation").
			Where("monitor_id = ? AND probe_id = ? AND active = ?", hb.MonitorID, hb.ProbeID, true)
		if tx.Dialect().Name() == dialect.MySQL {
			member = member.For("UPDATE")
		}
		if err := member.Scan(ctx, &activeGeneration); err != nil {
			return probeRegistryError(err)
		}
		if activeGeneration != hb.AssignmentGeneration {
			return ports.ErrConflict
		}
		var activeRevision int64
		activeConfigQuery := tx.NewSelect().TableExpr("probe_active_configs").Column("revision").Where("probe_id = ?", hb.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			activeConfigQuery = activeConfigQuery.For("UPDATE")
		}
		if err := activeConfigQuery.Scan(ctx, &activeRevision); err == nil {
			if activeRevision > 0 && hb.ConfigRevision != activeRevision {
				return ports.ErrConflict
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		previous := new(monitorProbeStateModel)
		stateQuery := tx.NewSelect().Model(previous).Where("monitor_id = ? AND probe_id = ?", hb.MonitorID, hb.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			stateQuery = stateQuery.For("UPDATE")
		}
		err = stateQuery.Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			previous = nil
			if commit.ExpectedStateSeq != 0 {
				return ports.ErrStaleLocalState
			}
		} else if err != nil {
			return err
		} else if previous.Seq != commit.ExpectedStateSeq {
			return ports.ErrStaleLocalState
		}

		observation := domain.RegionalObservation{
			MonitorID: hb.MonitorID, ProbeID: hb.ProbeID, AssignmentGeneration: hb.AssignmentGeneration,
			StreamID: hb.StreamID, Seq: hb.SourceSeq, ConfigRevision: hb.ConfigRevision,
			Status: hb.Status, RawStatus: commit.RawStatus, DownCount: hb.DownCount,
			Ping: hb.Ping, DurationMS: hb.Duration, Message: hb.Msg, Important: hb.Important,
			ObservedAt: hb.Time, ReceivedAt: hb.ReceivedAt,
		}
		state := domain.RegionalState{
			MonitorID: hb.MonitorID, ProbeID: hb.ProbeID, AssignmentGeneration: hb.AssignmentGeneration,
			StreamID: hb.StreamID, Seq: hb.SourceSeq, ConfigRevision: hb.ConfigRevision,
			Status: hb.Status, DownCount: hb.DownCount, ObservedAt: hb.Time, ReceivedAt: hb.ReceivedAt,
		}
		if hb.Status == domain.StatusUp {
			t := hb.Time
			state.LastSuccessAt = &t
		} else if previous != nil && previous.AssignmentGeneration == hb.AssignmentGeneration {
			state.LastSuccessAt = utcTimePtr(previous.LastSuccessAt)
		}
		model := HeartbeatModelFromDomain(&hb)
		if _, err := tx.NewInsert().Model(model).Exec(ctx); err != nil {
			return fmt.Errorf("insert local heartbeat: %w", probeRegistryError(err))
		}
		hb.ID = model.ID
		obs := observationModel(observation)
		if _, err := tx.NewInsert().Model(&obs).Exec(ctx); err != nil {
			return fmt.Errorf("insert local observation: %w", probeRegistryError(err))
		}
		if err := upsertRegionalState(ctx, tx, state); err != nil {
			return err
		}
		if err := markDirtyTx(ctx, tx, domain.DirtyBucketsForObservation(observation)); err != nil {
			return err
		}
		return commitLifecycleAndDeliveriesTx(ctx, tx, observation, &commit)
	})
	if err != nil {
		return nil, err
	}
	if callerIncident != nil && commit.Incident != nil {
		callerIncident.HubIncidentID = commit.Incident.HubIncidentID
		callerIncident.SourceAlertID = commit.Incident.SourceAlertID
		callerIncident.TransitionVersion = commit.Incident.TransitionVersion
	}
	if callerAlert != nil && commit.Alert != nil {
		*callerAlert = *commit.Alert
	}
	if callerEscalation != nil && commit.Escalation != nil {
		*callerEscalation = *commit.Escalation
	}
	return &hb, nil
}

// advanceExplicitLocalSequence keeps low-level, explicitly sequenced commits
// from leaving the allocator behind. Its update also serializes those commits
// with CommitLocalHeartbeat. Neither path may recreate a missing allocator.
func advanceExplicitLocalSequence(ctx context.Context, tx bun.Tx, obs domain.RegionalObservation) error {
	if obs.StreamID != domain.LocalStreamID {
		return nil
	}
	if obs.ProbeID != domain.LocalProbeID {
		return fmt.Errorf("reserved local stream: %w", domain.ErrValidation)
	}
	if _, err := tx.NewUpdate().TableExpr("probe_local_sequence").
		Set("last_seq = CASE WHEN last_seq < ? THEN ? ELSE last_seq END", obs.Seq, obs.Seq).
		Where("id = 1").Exec(ctx); err != nil {
		return err
	}
	var seq int64
	if err := tx.NewSelect().TableExpr("probe_local_sequence").Column("last_seq").Where("id = 1").Scan(ctx, &seq); err != nil {
		return err
	}
	return nil
}
