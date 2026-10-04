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

// probeOperationModel stores one durable administrative operation receipt.
// No secret, token or credential column exists here by construction.
type probeOperationModel struct {
	bun.BaseModel `bun:"table:probe_operations,alias:po"`
	Error         *domain.ProbeOperationError `bun:"-"`
	OperationID   string                      `bun:"operation_id,pk"`
	ProbeID       string                      `bun:"probe_id"`
	Kind          string                      `bun:"kind"`
	Status        string                      `bun:"status"`
	Phase         string                      `bun:"phase"`
	ErrorCode     *string                     `bun:"error_code"`
	ErrorMessage  *string                     `bun:"error_message"`
	CreatedAt     time.Time                   `bun:"created_at"`
	UpdatedAt     time.Time                   `bun:"updated_at"`
}

func (m probeOperationModel) domain() domain.ProbeOperation {
	out := domain.ProbeOperation{
		OperationID: m.OperationID, ProbeID: m.ProbeID, Kind: m.Kind,
		Status: m.Status, Phase: m.Phase, CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
	}
	if m.ErrorCode != nil && m.ErrorMessage != nil {
		out.Error = &domain.ProbeOperationError{Code: *m.ErrorCode, Message: *m.ErrorMessage}
	}
	return out
}

func probeOperationRow(o domain.ProbeOperation) probeOperationModel {
	m := probeOperationModel{
		OperationID: o.OperationID, ProbeID: o.ProbeID, Kind: o.Kind,
		Status: o.Status, Phase: o.Phase, CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(),
	}
	if o.Error != nil {
		code, message := o.Error.Code, o.Error.Message
		m.ErrorCode, m.ErrorMessage = &code, &message
	}
	return m
}

// ProbeOperationStore is the dialect-neutral durable operation receipt store.
// One implementation serves MariaDB and SQLite with identical semantics.
type ProbeOperationStore struct{ db *bun.DB }

// NewProbeOperationStore creates a Bun-backed operation store.
func NewProbeOperationStore(db *bun.DB) *ProbeOperationStore { return &ProbeOperationStore{db: db} }

var _ ports.ProbeOperationRepository = (*ProbeOperationStore)(nil)

// CreateOperation commits one receipt. The row must exist before any caller
// acknowledges work; a duplicate identity is a conflict, never a silent
// overwrite of an earlier operation.
func (s *ProbeOperationStore) CreateOperation(ctx context.Context, operation domain.ProbeOperation) error {
	if s == nil || s.db == nil || !domain.ValidProbeOperation(operation) {
		return fmt.Errorf("invalid probe operation: %w", domain.ErrValidation)
	}
	row := probeOperationRow(operation)
	if _, err := s.db.NewInsert().Model(&row).Exec(ctx); err != nil {
		return fmt.Errorf("create probe operation: %w", probeRegistryError(err))
	}
	return nil
}

// GetOperation returns one exact receipt. Reads never create rows.
func (s *ProbeOperationStore) GetOperation(ctx context.Context, operationID string) (*domain.ProbeOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe operation id: %w", domain.ErrValidation)
	}
	row := new(probeOperationModel)
	if err := s.db.NewSelect().Model(row).Where("operation_id = ?", operationID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, fmt.Errorf("read probe operation: %w", err)
	}
	out := row.domain()
	return &out, nil
}

// FinishOperation records the terminal hub-side outcome. Terminal receipts are
// immutable history: finishing an already finished operation is a conflict.
func (s *ProbeOperationStore) FinishOperation(ctx context.Context, operationID string, status, phase string, opErr *domain.ProbeOperationError, at time.Time) error {
	if s == nil || s.db == nil || at.IsZero() || !domain.ValidProbeOperationID(operationID) ||
		!domain.ValidProbeOperationPhase(phase) ||
		(status != domain.ProbeOperationSucceeded && status != domain.ProbeOperationFailed) ||
		(status == domain.ProbeOperationFailed && (opErr == nil || !domain.ValidProbeOperationError(*opErr))) ||
		(status == domain.ProbeOperationSucceeded && opErr != nil) {
		return fmt.Errorf("invalid probe operation outcome: %w", domain.ErrValidation)
	}
	var code, message any
	if opErr != nil {
		code, message = opErr.Code, opErr.Message
	}
	return s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		row := new(probeOperationModel)
		if err := tx.NewSelect().Model(row).Where("operation_id = ?", operationID).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return err
		}
		if row.Status == domain.ProbeOperationSucceeded || row.Status == domain.ProbeOperationFailed {
			return ports.ErrConflict
		}
		if _, err := tx.NewUpdate().Model(row).
			Set("status = ?", status).Set("phase = ?", phase).
			Set("error_code = ?", code).Set("error_message = ?", message).
			Set("updated_at = ?", at.UTC()).
			Where("operation_id = ?", operationID).Exec(ctx); err != nil {
			return err
		}
		return nil
	})
}
