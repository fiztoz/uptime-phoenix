package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const preparationRearmCooldown = time.Minute

type preparationRoundModel struct {
	bun.BaseModel      `bun:"table:probe_delivery_preparation_rounds"`
	MonitorID          int64 `bun:",pk"`
	RequestRound       int64 `bun:",pk"`
	AssignmentRevision int64
	RoundID            string
	HubID              string
	AlertDelivery      domain.AlertDelivery
	CreatedAt          time.Time
	ExpiresAt          time.Time
}
type preparationMemberModel struct {
	bun.BaseModel `bun:"table:probe_delivery_preparation_members"`
	MonitorID     int64  `bun:",pk"`
	RequestRound  int64  `bun:",pk"`
	ProbeID       string `bun:",pk"`
	Generation    int64
}
type preparationRequestModel struct {
	bun.BaseModel  `bun:"table:probe_delivery_preparation_requests"`
	RequestID      string `bun:",pk"`
	MonitorID      int64
	RequestRound   int64
	ProbeID        string
	Generation     int64
	ConfigRevision int64
	StreamID       string
	SHA256         string
	CommandID      *string
	PreparedAt     time.Time
}

func (r preparationRoundModel) round(members []preparationMemberModel) domain.DeliveryPreparationRound {
	out := domain.DeliveryPreparationRound{HubID: r.HubID, RoundID: r.RoundID, MonitorID: r.MonitorID, RequestRound: r.RequestRound, AssignmentRevision: r.AssignmentRevision, AlertDelivery: r.AlertDelivery, CreatedAt: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), Members: make([]domain.DeliveryPreparationMember, len(members))}
	for i, m := range members {
		out.Members[i] = domain.DeliveryPreparationMember{ProbeID: m.ProbeID, Generation: m.Generation}
	}
	return out
}

// selectPreparationRoundTx validates stored membership before considering expiry
// or rearm. All assigned members are captured, not just live/reporting sources.
// SERIALIZABLE predicate locks and the integer PK serialize competing creators;
// runConfigAuthorityTx retries InnoDB serialization/deadlock failures.
func selectPreparationRoundTx(ctx context.Context, tx bun.Tx, hub string, monitor, revision int64, mode domain.AlertDelivery, now time.Time) (preparationRoundModel, error) {
	set, err := readProbeAssignments(ctx, tx, monitor)
	if err != nil {
		return preparationRoundModel{}, err
	}
	if set.Revision != revision || set.AlertDelivery != mode || (mode != domain.AlertDeliveryAggregate && mode != domain.AlertDeliveryBoth) {
		return preparationRoundModel{}, ports.ErrConflict
	}
	var latest preparationRoundModel
	err = tx.NewSelect().Model(&latest).Where("monitor_id = ?", monitor).Order("request_round DESC").Limit(1).Scan(ctx)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return preparationRoundModel{}, err
	}
	if err == nil {
		var members []preparationMemberModel
		if err := tx.NewSelect().Model(&members).Where("monitor_id = ? AND request_round = ?", monitor, latest.RequestRound).Order("probe_id ASC").Scan(ctx); err != nil {
			return preparationRoundModel{}, err
		}
		if !domain.ValidDeliveryPreparationRound(latest.round(members)) || latest.HubID != hub {
			return preparationRoundModel{}, ports.ErrConflict
		}
		if latest.AssignmentRevision == revision {
			if latest.AlertDelivery != mode || len(members) != len(set.Assignments) {
				return preparationRoundModel{}, ports.ErrConflict
			}
			for i, m := range members {
				if m.ProbeID != set.Assignments[i].ProbeID || m.Generation != set.Assignments[i].Generation {
					return preparationRoundModel{}, ports.ErrConflict
				}
			}
			if now.Before(latest.CreatedAt.UTC()) {
				return preparationRoundModel{}, ports.ErrConflict
			} // Clock rollback supplies no new request permit.
			due := !now.Before(latest.ExpiresAt.UTC())
			if !due && !now.Before(latest.CreatedAt.UTC().Add(preparationRearmCooldown)) {
				due, err = currentPreparationFailureTx(ctx, tx, latest, now)
				if err != nil {
					return preparationRoundModel{}, err
				}
			}
			if !due {
				return latest, nil
			}
		} else if latest.AssignmentRevision > revision {
			return preparationRoundModel{}, ports.ErrConflict
		}
	}
	if latest.RequestRound == math.MaxInt64 {
		return preparationRoundModel{}, ports.ErrConflict
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return preparationRoundModel{}, err
	}
	next := preparationRoundModel{RoundID: id.String(), MonitorID: monitor, RequestRound: latest.RequestRound + 1, AssignmentRevision: revision, HubID: hub, AlertDelivery: mode, CreatedAt: now.UTC().Truncate(time.Microsecond), ExpiresAt: now.UTC().Truncate(time.Microsecond).Add(deliveryPrepareLifetime)}
	members := make([]preparationMemberModel, len(set.Assignments))
	for i, a := range set.Assignments {
		members[i] = preparationMemberModel{MonitorID: monitor, RequestRound: next.RequestRound, ProbeID: a.ProbeID, Generation: a.Generation}
	}
	if !domain.ValidDeliveryPreparationRound(next.round(members)) {
		return preparationRoundModel{}, ports.ErrConflict
	}
	if _, err := tx.NewInsert().Model(&next).Exec(ctx); err != nil {
		return preparationRoundModel{}, err
	}
	if _, err := tx.NewInsert().Model(&members).Exec(ctx); err != nil {
		return preparationRoundModel{}, err
	}
	return next, nil
}

// Only current-document/current-accepted-stream failed requests rearm. Historical
// old-document or reset-stream outcomes may finish their own immutable ledger,
// but are not a reason to continually invalidate the new request round.
func currentPreparationFailureTx(ctx context.Context, tx bun.Tx, r preparationRoundModel, now time.Time) (bool, error) {
	var count int
	err := tx.NewSelect().TableExpr("probe_delivery_preparation_requests AS r").ColumnExpr("COUNT(*)").
		Join("JOIN probe_commands AS c ON c.command_id = r.command_id").
		Join("JOIN probe_active_configs AS a ON a.probe_id = r.probe_id AND a.revision = r.config_revision AND a.sha256 = r.sha256").
		Join("JOIN probe_connections AS p ON p.probe_id = r.probe_id AND p.stream_id = r.stream_id").
		Where("r.monitor_id = ? AND r.request_round = ?", r.MonitorID, r.RequestRound).
		Where("c.kind = ? AND c.hub_id = ? AND c.probe_id = r.probe_id AND c.stream_id = r.stream_id", "delivery.prepare", r.HubID).
		Where("(c.remote_confirmed = ? AND c.status IN (?, ?)) OR (c.status = ? AND c.expires_at <= ?)", true, "rejected", "expired", "expired", now.UTC()).Scan(ctx, &count)
	return count > 0, err
}

func preparationRoundRequestID(bound domain.ProbeDeliveryModeApplication, round preparationRoundModel) string {
	sum := sha256.Sum256([]byte("uptime-phoenix/delivery.prepare/round/" + round.RoundID + "/" + strconv.FormatInt(round.RequestRound, 10) + "/" + deliveryPreparationCommandID(bound)))
	var id [16]byte
	copy(id[:], sum[:])
	id[6] = (id[6] & 0x0f) | 0x80
	id[8] = (id[8] & 0x3f) | 0x80
	h := hex.EncodeToString(id[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func writePreparationRoundRequestTx(ctx context.Context, tx bun.Tx, r preparationRoundModel, bound domain.ProbeDeliveryModeApplication, requestID string, at time.Time, remote, allowInsert bool) error {
	if !domain.ValidProbeDeliveryModeApplication(bound) || bound.HubID != r.HubID || bound.MonitorID != r.MonitorID || bound.AssignmentRevision != r.AssignmentRevision || bound.AlertDelivery != r.AlertDelivery || requestID != preparationRoundRequestID(bound, r) || at.Before(r.CreatedAt.UTC()) || !at.Before(r.ExpiresAt.UTC()) {
		return ports.ErrConflict
	}
	row := preparationRequestModel{RequestID: requestID, MonitorID: bound.MonitorID, RequestRound: r.RequestRound, ProbeID: bound.ProbeID, Generation: bound.Generation, ConfigRevision: bound.ConfigRevision, StreamID: bound.StreamID, SHA256: bound.SHA256, PreparedAt: at.UTC().Truncate(time.Microsecond)}
	if remote {
		row.CommandID = &requestID
	}
	var existing preparationRequestModel
	err := tx.NewSelect().Model(&existing).Where("request_id = ?", requestID).Scan(ctx)
	if err == nil {
		// Retry ignores the new caller's clock but never rewrites stored provenance.
		if existing.RequestID != row.RequestID || existing.MonitorID != row.MonitorID || existing.RequestRound != row.RequestRound || existing.ProbeID != row.ProbeID || existing.Generation != row.Generation || existing.ConfigRevision != row.ConfigRevision || existing.StreamID != row.StreamID || existing.SHA256 != row.SHA256 || (remote && (existing.CommandID == nil || *existing.CommandID != requestID)) || (!remote && existing.CommandID != nil) || existing.PreparedAt.IsZero() || existing.PreparedAt.UTC().Before(r.CreatedAt.UTC()) || !existing.PreparedAt.UTC().Before(r.ExpiresAt.UTC()) {
			return ports.ErrConflict
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !allowInsert {
		return ports.ErrConflict
	}
	_, err = tx.NewInsert().Model(&row).Exec(ctx)
	return err
}

var _ ports.LocalDeliveryPreparationRoundReconciler = (*ProbeActivationStore)(nil)

// ReconcileLocalDeliveryPreparationRounds proves a selected committed local graph
// anew and writes pending requests only. No receipt retry, execution read, check or
// provider path calls it; request corruption cannot become suppression authority.
func (r *ProbeActivationStore) ReconcileLocalDeliveryPreparationRounds(ctx context.Context, want domain.ProbeActiveConfig) error {
	if r == nil || r.db == nil || r.encoder == nil || want.ProbeID != domain.LocalProbeID || !domain.ValidProbeActiveConfig(want) {
		return domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		def, err := readAppliedLocalTx(ctx, tx, r.encoder)
		if err != nil {
			return err
		}
		var active probeActiveConfigModel
		if err := tx.NewSelect().Model(&active).Where("probe_id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
			return err
		}
		if def.Target != want.ProbeConfigTarget || def.Revision != want.Revision || active.HubID != want.HubID || active.Revision != want.Revision || active.SHA256 != want.SHA256 || !active.AppliedAt.Equal(want.AppliedAt) || active.AssignmentCount != want.AssignmentCount || len(def.Assignments) != want.AssignmentCount {
			return ports.ErrConflict
		}
		var receipt probeConfigAppliedReceiptModel
		if err := tx.NewSelect().Model(&receipt).Where("probe_id = ? AND revision = ?", want.ProbeID, want.Revision).Scan(ctx); err != nil {
			return ports.ErrConflict
		}
		if receipt.HubID != active.HubID || receipt.SHA256 != active.SHA256 || receipt.AssignmentCount != active.AssignmentCount || !receipt.AppliedAt.Equal(active.AppliedAt) {
			return ports.ErrConflict
		}
		facts := make(map[int64]domain.ProbeDeliveryModeApplication, len(def.RegionalDeliveryApplications))
		for _, f := range def.RegionalDeliveryApplications {
			facts[f.MonitorID] = f
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		var deadlines []time.Time
		for _, member := range def.Assignments {
			if member.AlertDelivery != domain.AlertDeliveryAggregate && member.AlertDelivery != domain.AlertDeliveryBoth {
				continue
			}
			if member.Monitor == nil {
				return ports.ErrConflict
			}
			fact, ok := facts[member.Monitor.ID]
			if !ok || fact.AssignmentRevision != member.AssignmentRevision || fact.Generation != member.Generation {
				return ports.ErrConflict
			}
			round, err := selectPreparationRoundTx(ctx, tx, want.HubID, member.Monitor.ID, member.AssignmentRevision, member.AlertDelivery, now)
			if err != nil {
				return err
			}
			fact.AlertDelivery = member.AlertDelivery
			if err := writePreparationRoundRequestTx(ctx, tx, round, fact, preparationRoundRequestID(fact, round), now, false, true); err != nil {
				return err
			}
			deadlines = append(deadlines, round.ExpiresAt.UTC())
		}
		finalNow, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		for _, deadline := range deadlines {
			if !finalNow.Before(deadline) {
				return ports.ErrConflict
			}
		}
		return nil
	})
	return remoteSyncError(ctx, err)
}
