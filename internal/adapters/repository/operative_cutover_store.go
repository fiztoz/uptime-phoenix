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

// Operative aggregate/both cutover storage. This store coordinates full source
// barriers under independently stored DB-clock claims. It performs no socket or
// provider I/O, never logs protected payload or provider settings, and never
// treats the pure evaluator as stored permission.
//
// Distinctness: source ACKs, regional facts, delivery preparation, preparation
// rounds and aggregate_standby_* planning are separate evidence and are never
// relabeled here. Genuine aggregate incidents/intents live in
// operative_aggregate_* and are never promoted from held standby plans.

const (
	operativePhasePreparing = "preparing"
	operativePhaseQuiescing = "quiescing"
	operativePhaseQuiesced  = "quiesced"
	operativePhaseApplying  = "applying"
	operativePhaseActive    = "active"
	operativePhaseDraining  = "draining"
	operativePhaseReleased  = "released"
	operativePhaseBlocked   = "blocked"

	// operativeRearmCooldown is the minimum DB-clock delay before a rejected-busy
	// or expired cutover command may be reissued with a new immutable command ID
	// against the same barrier. Old outcomes are never rewritten.
	operativeRearmCooldown = time.Minute
	// operativeRetryDelay is the coordinator's due_at backoff while work is
	// legitimately pending. It is not an authority or lease duration.
	operativeRetryDelay = 5 * time.Second
	// operativeCommandLifetime is the fixed lifetime of a DB-clock-created
	// protected cutover command (bounded by ValidDeliveryCutoverCommand).
	operativeCommandLifetime = 24 * time.Hour
	// operativeBusyCode is the only rearmable refusal: the source persists its
	// hold first and then refuses busy while provider work is unresolved. It is
	// the exact code the real edge source emits (delivery_provider_busy); any
	// other refusal is corruption and blocks the barrier.
	operativeBusyCode = "delivery_provider_busy"
)

// OperativeDeliveryStore implements the hub-side operative cutover, local
// delivery authorization, aggregate delivery and source provider journal ports.
type OperativeDeliveryStore struct {
	db        *bun.DB
	codec     ports.DeliveryCutoverCodec
	protector ports.ProbeCommandProtector
	sync      *RemoteProbeConfigSyncStore
	encoder   ports.LocalProbeConfigEncoder
}

var (
	_ ports.AggregateCutoverRepository   = (*OperativeDeliveryStore)(nil)
	_ ports.AggregateDeliveryRepository  = (*OperativeDeliveryStore)(nil)
	_ ports.LocalDeliveryAuthorizer      = (*OperativeDeliveryStore)(nil)
	_ ports.SourceProviderAttemptJournal = (*OperativeDeliveryStore)(nil)
)

// NewOperativeDeliveryStore binds the shared hub database to the retained
// protected document machinery and the closed cutover codec.
//
// Dependencies are explicit: db is the hub engine; codec is the shared
// DeliveryCutoverCodec used to build protected command payloads; protector is
// the verified installation command protector (KeyHash/SealCommand/OpenCommand);
// sync is the remote config sync store used ONLY through its retained protected
// document/receipt verification (never mutable-source reconstruction); encoder
// is the local config encoder used to re-prove the exact whole-source graph.
// The sync store must be fully constructed (decoder + config protector set).
func NewOperativeDeliveryStore(db *bun.DB, codec ports.DeliveryCutoverCodec, protector ports.ProbeCommandProtector, sync *RemoteProbeConfigSyncStore, encoder ports.LocalProbeConfigEncoder) *OperativeDeliveryStore {
	return &OperativeDeliveryStore{db: db, codec: codec, protector: protector, sync: sync, encoder: encoder}
}

type operativeStateModel struct {
	bun.BaseModel      `bun:"table:operative_cutover_state,alias:opstate"`
	MonitorID          int64 `bun:"monitor_id,pk"`
	HubID              string
	OwnerID            string
	LeaseEpoch         int64
	LeaseUntil         time.Time
	DueAt              time.Time
	EvaluatedAt        *time.Time
	BarrierID          *string
	Phase              string
	AlertDelivery      domain.AlertDelivery
	AssignmentRevision int64
	TransitionVersion  int64
	Reason             string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type operativeBarrierModel struct {
	bun.BaseModel      `bun:"table:operative_cutover_barriers"`
	BarrierID          string `bun:"barrier_id,pk"`
	MonitorID          int64
	HubID              string
	AlertDelivery      domain.AlertDelivery
	AssignmentRevision int64
	RoundID            string
	RequestRound       int64
	CreatedAt          time.Time
	ActivatedAt        *time.Time
}

type operativeMemberModel struct {
	bun.BaseModel        `bun:"table:operative_cutover_members"`
	BarrierID            string `bun:",pk"`
	ProbeID              string `bun:",pk"`
	MonitorID            int64
	Generation           int64
	AssignmentRevision   int64
	ConfigRevision       int64
	SHA256               string
	StreamID             string
	AlertDelivery        domain.AlertDelivery
	PreparationCommandID string
	QuiesceCommandID     *string
	ApplyCommandID       *string
	ResumeCommandID      *string
	QuiescedAt           *time.Time
	AppliedAt            *time.Time
	ResumedAt            *time.Time
}

type operativeCommandModel struct {
	bun.BaseModel `bun:"table:operative_cutover_commands"`
	CommandID     string `bun:"command_id,pk"`
	BarrierID     string
	MonitorID     int64
	ProbeID       string
	Kind          string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	OutcomeStatus string
	OutcomeCode   string
	OutcomeAt     *time.Time
}

type operativeGateModel struct {
	bun.BaseModel `bun:"table:operative_source_gates"`
	ProbeID       string `bun:",pk"`
	MonitorID     int64  `bun:",pk"`
	BarrierID     string `bun:",pk"`
	Mode          domain.AlertDelivery
	State         string
	GrantedAt     time.Time
	ReleasedAt    *time.Time
}

type operativeLocalApplicationModel struct {
	bun.BaseModel      `bun:"table:operative_local_applications"`
	ProbeID            string `bun:",pk"`
	ConfigRevision     int64  `bun:",pk"`
	MonitorID          int64  `bun:",pk"`
	BarrierID          string `bun:",pk"`
	AssignmentRevision int64
	Generation         int64
	SHA256             string
	StreamID           string
	AlertDelivery      domain.AlertDelivery
	AppliedAt          time.Time
}

type operativeCapabilityModel struct {
	bun.BaseModel        `bun:"table:operative_source_capabilities"`
	ProbeID              string `bun:"probe_id,pk"`
	Capability           string
	ConnectionGeneration int64
	AttestedAt           time.Time
}

type operativeAttemptModel struct {
	bun.BaseModel  `bun:"table:operative_provider_attempts"`
	AttemptID      string `bun:"attempt_id,pk"`
	Scope          string
	MonitorID      int64
	ProbeID        string
	DeliveryID     string
	ClaimAttempt   int64
	NotificationID int64
	SettingsSHA256 string
	BudgetUntil    time.Time
	State          string
	CreatedAt      time.Time
	FinishedAt     *time.Time
}

func operativeStateFence(a operativeStateModel, f domain.AggregateStandbyLease, now time.Time) bool {
	return a.MonitorID == f.MonitorID && a.OwnerID == f.OwnerID && a.LeaseEpoch == f.Epoch && a.LeaseUntil.Equal(f.Until) && a.LeaseUntil.After(now)
}

// ClaimCutovers claims due operative work per monitor under independent stored
// DB-clock leases. Each expired acquisition advances the epoch, even for the
// same process owner. Claims are returned alongside per-monitor typed errors;
// committed claims must still be processed by the caller.
func (r *OperativeDeliveryStore) ClaimCutovers(ctx context.Context, owner string, ttl time.Duration, limit int) ([]domain.AggregateStandbyLease, error) {
	if r == nil || r.db == nil || !validStandbyUUID(owner) || ttl < time.Second || ttl > time.Minute || limit < 1 || limit > 100 {
		return nil, domain.ErrValidation
	}
	// The first transaction discovers selectors only, never authority. Each
	// monitor acquires independently so one corrupt/exhausted lease cannot roll
	// back healthy claims or strand a fleet batch.
	var ids []int64
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		ids = nil
		seen := map[int64]bool{}
		var fromSets []int64
		if err := tx.NewSelect().TableExpr("monitor_probe_assignment_sets AS s").ColumnExpr("s.monitor_id").
			Join("LEFT JOIN operative_cutover_state AS a ON a.monitor_id=s.monitor_id").
			Where("(s.alert_delivery IN (?,?) OR a.phase IN (?,?,?,?,?,?,?,?))", domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth,
				operativePhasePreparing, operativePhaseQuiescing, operativePhaseQuiesced, operativePhaseApplying, operativePhaseActive, operativePhaseDraining, operativePhaseReleased, operativePhaseBlocked).
			Where("a.monitor_id IS NULL OR (a.lease_until <= ? AND a.due_at <= ?)", now, now).
			OrderExpr("a.due_at ASC,s.monitor_id ASC").Limit(int64(limit)).Scan(ctx, &fromSets); err != nil {
			return err
		}
		for _, id := range fromSets {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		// A deleted or replaced assignment set must never strand an unfinished
		// barrier: those monitors stay selectable until they drain or block.
		if len(ids) < limit {
			var orphaned []int64
			if err := tx.NewSelect().Table("operative_cutover_state").Column("monitor_id").
				Where("phase IN (?,?,?,?,?,?,?,?)", operativePhasePreparing, operativePhaseQuiescing, operativePhaseQuiesced, operativePhaseApplying, operativePhaseActive, operativePhaseDraining, operativePhaseReleased, operativePhaseBlocked).
				Where("lease_until <= ? AND due_at <= ?", now, now).
				Where("monitor_id NOT IN (SELECT monitor_id FROM monitor_probe_assignment_sets)").
				OrderExpr("due_at ASC, monitor_id ASC").Limit(int64(limit-len(ids))).Scan(ctx, &orphaned); err != nil {
				return err
			}
			for _, id := range orphaned {
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, remoteSyncError(ctx, err)
	}
	var claims []domain.AggregateStandbyLease
	var failed error
	for _, id := range ids {
		if ctx.Err() != nil {
			return claims, errors.Join(failed, ctx.Err())
		}
		var claim *domain.AggregateStandbyLease
		err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
			claim = nil
			if _, err := tx.ExecContext(ctx, "UPDATE monitors SET id=id WHERE id=?", id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE operative_cutover_state SET lease_epoch=lease_epoch WHERE monitor_id=?", id); err != nil {
				return err
			}
			var a operativeStateModel
			err := tx.NewSelect().Model(&a).Where("monitor_id=?", id).Scan(ctx)
			exists := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			// Recheck current eligibility, not the earlier selector snapshot.
			eligible, err := operativeClaimEligibleTx(ctx, tx, id, exists && a.Phase != operativePhasePreparing)
			if err != nil {
				return err
			}
			if !eligible {
				return nil
			}
			var hub string
			if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id=1").Scan(ctx, &hub); err != nil {
				return err
			}
			if !validStandbyUUID(hub) {
				return ports.ErrConflict
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			if exists {
				if a.HubID != hub || a.LeaseEpoch <= 0 || a.LeaseEpoch == math.MaxInt64 || a.TransitionVersion < 0 {
					return ports.ErrConflict
				}
				if a.LeaseUntil.After(now) || a.DueAt.After(now) {
					return nil
				}
			} else {
				a = operativeStateModel{MonitorID: id, HubID: hub, Phase: operativePhasePreparing, AlertDelivery: domain.AlertDeliveryRegional}
				a.CreatedAt, a.UpdatedAt = now, now
			}
			a.OwnerID = owner
			a.LeaseEpoch++
			a.LeaseUntil = now.Add(ttl)
			a.DueAt = now
			if exists {
				_, err = tx.NewUpdate().Model(&a).WherePK().Exec(ctx)
			} else {
				_, err = tx.NewInsert().Model(&a).Exec(ctx)
			}
			if err != nil {
				return err
			}
			claim = &domain.AggregateStandbyLease{MonitorID: id, OwnerID: owner, Epoch: a.LeaseEpoch, Until: a.LeaseUntil}
			return nil
		})
		if err != nil {
			failed = errors.Join(failed, remoteSyncError(ctx, err))
			continue
		}
		if claim != nil {
			claims = append(claims, *claim)
		}
	}
	return claims, failed
}

// operativeClaimEligibleTx rechecks that the monitor still needs operative work:
// an unfinished barrier (including a released one awaiting re-activation for a
// still-aggregate desired set) or a current aggregate/both desired set. Blocked
// barriers stay selectable so explicit disable can drain their original members.
func operativeClaimEligibleTx(ctx context.Context, tx bun.Tx, monitorID int64, barrierWork bool) (bool, error) {
	var state string
	err := tx.NewSelect().Table("monitor_probe_assignment_sets").Column("alert_delivery").Where("monitor_id=?", monitorID).Scan(ctx, &state)
	if err == nil {
		mode, ok := domain.CanonicalAlertDelivery(domain.AlertDelivery(state))
		if ok && (mode == domain.AlertDeliveryAggregate || mode == domain.AlertDeliveryBoth) {
			return true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	return barrierWork, nil
}

// ReconcileCutover advances one monitor's operative barrier under its stored
// claim. enabled=false drains existing work but can never activate a new
// barrier or silently restore regional starts. Every phase transition rechecks
// owner/epoch/expiry, the current set, generations, streams, exact retained
// configuration and prerequisites atomically on the DB clock.
func (r *OperativeDeliveryStore) ReconcileCutover(ctx context.Context, f domain.AggregateStandbyLease, enabled bool, evaluator ports.AggregateCutoverEvaluator) (domain.AggregateCutoverProgress, error) {
	var result domain.AggregateCutoverProgress
	if r == nil || r.db == nil || r.codec == nil || r.protector == nil || r.sync == nil || r.encoder == nil || evaluator == nil ||
		f.MonitorID <= 0 || !validStandbyUUID(f.OwnerID) || f.Epoch <= 0 || f.Until.IsZero() {
		return result, domain.ErrValidation
	}
	result.MonitorID = f.MonitorID
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		result = domain.AggregateCutoverProgress{MonitorID: f.MonitorID}
		if _, err := tx.ExecContext(ctx, "UPDATE operative_cutover_state SET lease_epoch=lease_epoch WHERE monitor_id=?", f.MonitorID); err != nil {
			return err
		}
		var state operativeStateModel
		if err := tx.NewSelect().Model(&state).Where("monitor_id=?", f.MonitorID).Scan(ctx); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !operativeStateFence(state, f, now) || state.TransitionVersion < 0 || state.EvaluatedAt != nil && now.Before(state.EvaluatedAt.UTC()) {
			return ports.ErrConflict
		}
		var hub string
		if err := tx.NewRaw("SELECT hub_id FROM probe_installation WHERE id=1").Scan(ctx, &hub); err != nil {
			return err
		}
		if !validStandbyUUID(hub) || state.HubID != hub {
			return ports.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "UPDATE monitors SET id=id WHERE id=?", f.MonitorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision=revision WHERE monitor_id=?", f.MonitorID); err != nil {
			return err
		}
		changed, err := r.reconcilePhaseTx(ctx, tx, &state, hub, enabled, evaluator, now)
		if err != nil {
			return err
		}
		after, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !operativeStateFence(state, f, after) || after.Before(now) {
			return ports.ErrConflict
		}
		state.EvaluatedAt = &now
		state.LeaseUntil = after
		state.DueAt = after.Add(operativeRetryDelay)
		state.UpdatedAt = after
		if _, err := tx.NewUpdate().Model(&state).WherePK().Exec(ctx); err != nil {
			return err
		}
		result = domain.AggregateCutoverProgress{MonitorID: f.MonitorID, BarrierID: barrierIDOf(state), Phase: state.Phase, Changed: changed}
		return nil
	})
	if err != nil {
		return domain.AggregateCutoverProgress{MonitorID: f.MonitorID}, remoteSyncError(ctx, err)
	}
	return result, nil
}

func barrierIDOf(state operativeStateModel) string {
	if state.BarrierID == nil {
		return ""
	}
	return *state.BarrierID
}

// reconcilePhaseTx is the pure-of-network barrier state machine. It returns
// whether the persisted phase or barrier content changed in this pass.
func (r *OperativeDeliveryStore) reconcilePhaseTx(ctx context.Context, tx bun.Tx, state *operativeStateModel, hub string, enabled bool, evaluator ports.AggregateCutoverEvaluator, now time.Time) (bool, error) {
	switch state.Phase {
	case operativePhaseBlocked:
		if !enabled && state.BarrierID != nil {
			return r.enterDrainingTx(state, now, "feature_disabled")
		}
		return false, nil
	case operativePhaseReleased:
		// Terminal for the finished barrier. A still-desired aggregate/both set
		// may start a NEW barrier; that is a fresh preparing pass, never a
		// rebind or a bypass of the completed drain.
		if !enabled {
			return false, nil
		}
		mode, ok := domain.CanonicalAlertDelivery(state.AlertDelivery)
		set, err := readProbeAssignments(ctx, tx, state.MonitorID)
		if err != nil || set == nil {
			if errors.Is(err, ports.ErrNotFound) {
				return false, nil
			}
			return false, err
		}
		current, valid := domain.CanonicalAlertDelivery(set.AlertDelivery)
		if !ok || !valid || mode == domain.AlertDeliveryRegional || current != mode || current == domain.AlertDeliveryRegional {
			return false, nil
		}
		state.BarrierID = nil
		state.Phase = operativePhasePreparing
		state.Reason = ""
		state.AssignmentRevision = 0
		return true, nil
	case operativePhasePreparing:
		if !enabled {
			return false, nil
		}
		return r.preparingTx(ctx, tx, state, hub, now)
	case operativePhaseQuiescing, operativePhaseQuiesced, operativePhaseApplying:
		// A disabled opt-in DRAINS an in-flight barrier instead of progressing it
		// to active: no new apply success, no activation and no aggregate
		// admission while disabled. Persisted gates stay enforced throughout the
		// drain; regional work returns only after every member's resume commits.
		if !enabled {
			return r.enterDrainingTx(state, now, "feature_disabled")
		}
		return r.progressTx(ctx, tx, state, hub, evaluator, now)
	case operativePhaseActive:
		return r.activeTx(ctx, tx, state, hub, enabled, evaluator, now)
	case operativePhaseDraining:
		return r.drainingTx(ctx, tx, state, hub, now)
	default:
		return false, ports.ErrConflict
	}
}
