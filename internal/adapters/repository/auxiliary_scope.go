package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// AuxiliaryScope binds capacity and certificate state to an assignment. Its
// zero value is the compatibility view of the current local assignment.
type AuxiliaryScope struct {
	ProbeID    string
	Generation int64
}

// NewAuxiliaryScope validates an explicit storage identity.
func NewAuxiliaryScope(probeID string, generation int64) (AuxiliaryScope, error) {
	if (probeID != domain.LocalProbeID && !validRemoteProbeID(probeID)) || generation < 1 {
		return AuxiliaryScope{}, fmt.Errorf("invalid auxiliary assignment: %w", domain.ErrValidation)
	}
	return AuxiliaryScope{ProbeID: probeID, Generation: generation}, nil
}

// Filter selects an exact assignment or the current local compatibility view.
// A removed local assignment never falls back to its earlier generation.
func (s AuxiliaryScope) Filter(q *bun.SelectQuery, alias string) *bun.SelectQuery {
	if s.ProbeID != "" {
		return q.Where("?.probe_id = ? AND ?.assignment_generation = ?", bun.Ident(alias), s.ProbeID, bun.Ident(alias), s.Generation)
	}
	return q.Where("?.probe_id = 'local'", bun.Ident(alias)).Where(`(
		EXISTS (SELECT 1 FROM monitor_probe_assignments AS a
		 WHERE a.monitor_id = ?.monitor_id AND a.probe_id = 'local'
		 AND a.active = 1 AND a.generation = ?.assignment_generation)
		OR (?.assignment_generation = 1 AND NOT EXISTS
		 (SELECT 1 FROM monitor_probe_assignment_sets AS s WHERE s.monitor_id = ?.monitor_id))
	)`, bun.Ident(alias), bun.Ident(alias), bun.Ident(alias), bun.Ident(alias))
}

// Resolve returns the write identity for a bound or compatibility repository.
func (s AuxiliaryScope) Resolve(ctx context.Context, db *bun.DB, monitorID int64) (AuxiliaryScope, error) {
	if s.ProbeID != "" {
		return s, nil
	}
	var generation int64
	err := db.NewSelect().TableExpr("monitor_probe_assignments").Column("generation").
		Where("monitor_id = ? AND probe_id = 'local' AND active = 1", monitorID).Scan(ctx, &generation)
	if err == nil {
		return AuxiliaryScope{ProbeID: domain.LocalProbeID, Generation: generation}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return AuxiliaryScope{}, err
	}
	exists, err := db.NewSelect().TableExpr("monitor_probe_assignment_sets").Where("monitor_id = ?", monitorID).Exists(ctx)
	if err != nil {
		return AuxiliaryScope{}, err
	}
	if exists {
		return AuxiliaryScope{}, fmt.Errorf("no active local assignment: %w", ports.ErrConflict)
	}
	return AuxiliaryScope{ProbeID: domain.LocalProbeID, Generation: 1}, nil
}
