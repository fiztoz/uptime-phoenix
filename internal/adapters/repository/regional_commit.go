package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeObservationModel struct {
	bun.BaseModel        `bun:"table:probe_observations,alias:obs"`
	ID                   int64     `bun:"id,pk,autoincrement"`
	MonitorID            int64     `bun:"monitor_id"`
	ProbeID              string    `bun:"probe_id"`
	AssignmentGeneration int64     `bun:"assignment_generation"`
	StreamID             string    `bun:"stream_id"`
	Seq                  int64     `bun:"seq"`
	ConfigRevision       int64     `bun:"config_revision"`
	Status               int       `bun:"status"`
	RawStatus            int       `bun:"raw_status"`
	DownCount            int       `bun:"down_count"`
	Ping                 int       `bun:"ping"`
	DurationMS           int       `bun:"duration_ms"`
	Message              string    `bun:"message"`
	Important            bool      `bun:"important"`
	ObservedAt           time.Time `bun:"observed_at"`
	ReceivedAt           time.Time `bun:"received_at"`
}

type monitorProbeStateModel struct {
	bun.BaseModel        `bun:"table:monitor_probe_state,alias:state"`
	MonitorID            int64      `bun:"monitor_id,pk"`
	ProbeID              string     `bun:"probe_id,pk"`
	AssignmentGeneration int64      `bun:"assignment_generation"`
	StreamID             string     `bun:"stream_id"`
	Seq                  int64      `bun:"seq"`
	ConfigRevision       int64      `bun:"config_revision"`
	Status               int        `bun:"status"`
	DownCount            int        `bun:"down_count"`
	ObservedAt           time.Time  `bun:"observed_at"`
	ReceivedAt           time.Time  `bun:"received_at"`
	LastSuccessAt        *time.Time `bun:"last_success_at"`
}

type probeStreamModel struct {
	bun.BaseModel `bun:"table:probe_streams,alias:stream"`
	ProbeID       string     `bun:"probe_id,pk"`
	StreamID      string     `bun:"stream_id,pk"`
	CommittedSeq  int64      `bun:"committed_seq"`
	RetiredAt     *time.Time `bun:"retired_at"`
	CreatedAt     time.Time  `bun:"created_at"`
	UpdatedAt     time.Time  `bun:"updated_at"`
}

func (m probeObservationModel) observation() domain.RegionalObservation {
	return domain.RegionalObservation{
		ID: m.ID, MonitorID: m.MonitorID, ProbeID: m.ProbeID, AssignmentGeneration: m.AssignmentGeneration,
		StreamID: m.StreamID, Seq: m.Seq, ConfigRevision: m.ConfigRevision,
		Status: domain.Status(m.Status), RawStatus: domain.Status(m.RawStatus),
		DownCount: m.DownCount, Ping: m.Ping, DurationMS: m.DurationMS, Message: m.Message,
		Important: m.Important, ObservedAt: m.ObservedAt.UTC(), ReceivedAt: m.ReceivedAt.UTC(),
	}
}

func (m monitorProbeStateModel) state() domain.RegionalState {
	out := domain.RegionalState{
		MonitorID: m.MonitorID, ProbeID: m.ProbeID, AssignmentGeneration: m.AssignmentGeneration,
		StreamID: m.StreamID, Seq: m.Seq, ConfigRevision: m.ConfigRevision,
		Status: domain.Status(m.Status), DownCount: m.DownCount,
		ObservedAt: m.ObservedAt.UTC(), ReceivedAt: m.ReceivedAt.UTC(),
	}
	if m.LastSuccessAt != nil {
		t := m.LastSuccessAt.UTC()
		out.LastSuccessAt = &t
	}
	return out
}

// RegionalCommitStore persists per-probe observations and current state.
// It is not wired into HeartbeatService or the scheduler.
type RegionalCommitStore struct{ db *bun.DB }

// NewRegionalCommitStore creates a dialect-neutral regional commit store.
func NewRegionalCommitStore(db *bun.DB) *RegionalCommitStore { return &RegionalCommitStore{db: db} }

var (
	_ ports.RegionalCommitRepository = (*RegionalCommitStore)(nil)
	_ ports.ProbeIngestRepository    = (*RegionalCommitStore)(nil)
)

// Commit writes one already-evaluated regional sample and replaces current state.
func (r *RegionalCommitStore) Commit(ctx context.Context, commit domain.RegionalCommit) error {
	if err := validateRegionalCommit(commit); err != nil {
		return err
	}
	return r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if err := requireActiveAssignment(ctx, tx, commit.Observation.MonitorID, commit.Observation.ProbeID, commit.Observation.AssignmentGeneration); err != nil {
			return err
		}
		obs := observationModel(commit.Observation)
		if _, err := tx.NewInsert().Model(&obs).Exec(ctx); err != nil {
			return fmt.Errorf("insert observation: %w", probeRegistryError(err))
		}
		return upsertRegionalState(ctx, tx, commit.State)
	})
}

// GetState returns current evidence for one assignment, or ErrNotFound.
func (r *RegionalCommitStore) GetState(ctx context.Context, monitorID int64, probeID string) (*domain.RegionalState, error) {
	m := new(monitorProbeStateModel)
	if err := r.db.NewSelect().Model(m).Where("monitor_id = ? AND probe_id = ?", monitorID, probeID).Scan(ctx); err != nil {
		return nil, fmt.Errorf("get regional state: %w", probeRegistryError(err))
	}
	state := m.state()
	return &state, nil
}

// ListStates returns current evidence for one monitor, ordered by probe_id.
func (r *RegionalCommitStore) ListStates(ctx context.Context, monitorID int64) ([]domain.RegionalState, error) {
	var rows []monitorProbeStateModel
	if err := r.db.NewSelect().Model(&rows).
		Where("monitor_id = ?", monitorID).
		OrderExpr("probe_id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list regional states: %w", err)
	}
	out := make([]domain.RegionalState, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.state())
	}
	return out, nil
}

// ListObservations returns ordered regional history in [from, to].
func (r *RegionalCommitStore) ListObservations(ctx context.Context, monitorID int64, probeID string, from, to time.Time) ([]domain.RegionalObservation, error) {
	var rows []probeObservationModel
	if err := r.db.NewSelect().Model(&rows).
		Where("monitor_id = ? AND probe_id = ?", monitorID, probeID).
		Where("observed_at >= ?", from.UTC()).
		Where("observed_at <= ?", to.UTC()).
		Order("observed_at ASC", "id ASC").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("list regional observations: %w", err)
	}
	out := make([]domain.RegionalObservation, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.observation())
	}
	return out, nil
}

// Ingest commits a contiguous remote prefix and updates per-probe state.
func (r *RegionalCommitStore) Ingest(ctx context.Context, batch domain.ProbeIngestBatch) (int64, error) {
	if batch.ProbeID == "" || batch.StreamID == "" || batch.FromSeq <= 0 || batch.ThroughSeq < batch.FromSeq || len(batch.Events) == 0 {
		return 0, fmt.Errorf("ingest batch: %w", domain.ErrValidation)
	}
	if int64(len(batch.Events)) != batch.ThroughSeq-batch.FromSeq+1 {
		return 0, fmt.Errorf("ingest coverage: %w", domain.ErrValidation)
	}
	var committed int64
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		now := time.Now().UTC()
		stream := new(probeStreamModel)
		err := tx.NewSelect().Model(stream).Where("probe_id = ? AND stream_id = ?", batch.ProbeID, batch.StreamID).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			stream = &probeStreamModel{ProbeID: batch.ProbeID, StreamID: batch.StreamID, CommittedSeq: 0, CreatedAt: now, UpdatedAt: now}
			if _, err := tx.NewInsert().Model(stream).Exec(ctx); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		next := stream.CommittedSeq + 1
		for i, event := range batch.Events {
			seq := batch.FromSeq + int64(i)
			if event.Seq != seq || event.ProbeID != batch.ProbeID || event.StreamID != batch.StreamID {
				return ports.ErrConflict
			}
			if seq <= stream.CommittedSeq {
				continue
			}
			if seq != next {
				return ports.ErrConflict
			}
			if err := requireActiveAssignment(ctx, tx, event.MonitorID, event.ProbeID, event.AssignmentGeneration); err != nil {
				return err
			}
			obs := observationModel(event)
			if _, err := tx.NewInsert().Model(&obs).Exec(ctx); err != nil {
				return probeRegistryError(err)
			}
			state := domain.RegionalState{
				MonitorID: event.MonitorID, ProbeID: event.ProbeID, AssignmentGeneration: event.AssignmentGeneration,
				StreamID: event.StreamID, Seq: event.Seq, ConfigRevision: event.ConfigRevision,
				Status: event.Status, DownCount: event.DownCount, ObservedAt: event.ObservedAt, ReceivedAt: event.ReceivedAt,
			}
			if event.Status == domain.StatusUp {
				t := event.ObservedAt.UTC()
				state.LastSuccessAt = &t
			}
			if err := upsertRegionalState(ctx, tx, state); err != nil {
				return err
			}
			next = seq + 1
			stream.CommittedSeq = seq
		}
		stream.UpdatedAt = now
		if _, err := tx.NewUpdate().Model(stream).WherePK().Exec(ctx); err != nil {
			return err
		}
		committed = stream.CommittedSeq
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("ingest: %w", err)
	}
	return committed, nil
}

// GetCursor returns the durable ingest high-water mark, or zero for a new stream.
func (r *RegionalCommitStore) GetCursor(ctx context.Context, probeID, streamID string) (int64, error) {
	stream := new(probeStreamModel)
	err := r.db.NewSelect().Model(stream).Where("probe_id = ? AND stream_id = ?", probeID, streamID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get ingest cursor: %w", err)
	}
	return stream.CommittedSeq, nil
}

func observationModel(obs domain.RegionalObservation) probeObservationModel {
	return probeObservationModel{
		MonitorID: obs.MonitorID, ProbeID: obs.ProbeID, AssignmentGeneration: obs.AssignmentGeneration,
		StreamID: obs.StreamID, Seq: obs.Seq, ConfigRevision: obs.ConfigRevision,
		Status: int(obs.Status), RawStatus: int(obs.RawStatus), DownCount: obs.DownCount,
		Ping: obs.Ping, DurationMS: obs.DurationMS, Message: obs.Message, Important: obs.Important,
		ObservedAt: obs.ObservedAt.UTC(), ReceivedAt: obs.ReceivedAt.UTC(),
	}
}

func upsertRegionalState(ctx context.Context, tx bun.Tx, state domain.RegionalState) error {
	row := monitorProbeStateModel{
		MonitorID: state.MonitorID, ProbeID: state.ProbeID, AssignmentGeneration: state.AssignmentGeneration,
		StreamID: state.StreamID, Seq: state.Seq, ConfigRevision: state.ConfigRevision,
		Status: int(state.Status), DownCount: state.DownCount,
		ObservedAt: state.ObservedAt.UTC(), ReceivedAt: state.ReceivedAt.UTC(), LastSuccessAt: state.LastSuccessAt,
	}
	existing := new(monitorProbeStateModel)
	err := tx.NewSelect().Model(existing).Where("monitor_id = ? AND probe_id = ?", state.MonitorID, state.ProbeID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
		return err
	}
	if err != nil {
		return err
	}
	_, err = tx.NewUpdate().Model(&row).Where("monitor_id = ? AND probe_id = ?", state.MonitorID, state.ProbeID).Exec(ctx)
	return err
}

func requireActiveAssignment(ctx context.Context, tx bun.Tx, monitorID int64, probeID string, generation int64) error {
	var generationFound int64
	err := tx.NewSelect().Table("monitor_probe_assignments").Column("generation").
		Where("monitor_id = ? AND probe_id = ? AND active = ?", monitorID, probeID, true).
		Scan(ctx, &generationFound)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	if err != nil {
		return err
	}
	if generationFound != generation {
		return ports.ErrConflict
	}
	return nil
}

func validateRegionalCommit(commit domain.RegionalCommit) error {
	obs, state := commit.Observation, commit.State
	if obs.MonitorID <= 0 || obs.ProbeID == "" || obs.AssignmentGeneration <= 0 || obs.StreamID == "" || obs.Seq <= 0 {
		return fmt.Errorf("regional observation identity: %w", domain.ErrValidation)
	}
	if obs.MonitorID != state.MonitorID || obs.ProbeID != state.ProbeID || obs.AssignmentGeneration != state.AssignmentGeneration || obs.Seq != state.Seq {
		return fmt.Errorf("observation/state identity mismatch: %w", domain.ErrValidation)
	}
	if obs.ObservedAt.IsZero() || obs.ReceivedAt.IsZero() {
		return fmt.Errorf("regional observation time: %w", domain.ErrValidation)
	}
	return nil
}
