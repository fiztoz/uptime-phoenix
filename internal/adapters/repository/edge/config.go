package edge

import (
	"context"
	"crypto/subtle"
	"errors"
	"reflect"
	"slices"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type configRow struct {
	bun.BaseModel    `bun:"table:edge_config,alias:c"`
	Revision         int64 `bun:",pk"`
	HubID            string
	ProbeID          string
	SchemaVersion    int
	SHA256           string
	CreatedAt        int64
	EffectiveAt      int64
	AppliedAt        int64
	KeyConfirmation  string
	ProtectedPayload []byte
}

func (r configRow) active(assignments []domain.EdgeAssignmentIdentity) domain.EdgeActiveConfig {
	return domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{
		ProbeConfigMetadata: domain.ProbeConfigMetadata{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: r.HubID, ProbeID: r.ProbeID}, Revision: r.Revision, SchemaVersion: r.SchemaVersion, SHA256: r.SHA256, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), EffectiveAt: time.UnixMicro(r.EffectiveAt).UTC()},
		KeyConfirmation:     r.KeyConfirmation, ProtectedPayload: slices.Clone(r.ProtectedPayload), StoredAt: time.UnixMicro(r.AppliedAt).UTC()},
		Assignments: assignments, AppliedAt: time.UnixMicro(r.AppliedAt).UTC()}
}

// ActivateConfig changes the selected revision and scheduler index atomically.
// The caller must first authenticate and runtime-validate the complete document.
func (s *Store) ActivateConfig(ctx context.Context, config domain.EdgeActiveConfig) error {
	p := config.Snapshot
	if !domain.ValidProbeConfigMetadata(p.ProbeConfigMetadata) || p.ProbeID == domain.LocalProbeID || config.ConnectionGeneration <= 0 || config.AppliedAt.IsZero() || len(p.KeyConfirmation) != 64 || len(p.ProtectedPayload) <= domain.ProbeConfigProtectionOverhead || len(p.ProtectedPayload) > domain.MaxProbeConfigBytes+domain.ProbeConfigProtectionOverhead || len(config.Assignments) > 10000 {
		return domain.ErrValidation
	}
	assignments := slices.Clone(config.Assignments)
	slices.SortFunc(assignments, func(a, b domain.EdgeAssignmentIdentity) int {
		if a.MonitorID < b.MonitorID {
			return -1
		}
		if a.MonitorID > b.MonitorID {
			return 1
		}
		return 0
	})
	for n, a := range assignments {
		if a.MonitorID <= 0 || a.Generation <= 0 || n > 0 && a.MonitorID == assignments[n-1].MonitorID {
			return domain.ErrValidation
		}
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if i.HubID == "" || p.HubID != i.HubID || p.ProbeID != i.ProbeID || p.Revision < i.ConfigRevision || config.ConnectionGeneration != i.ConnectionGeneration {
			return ports.ErrConflict
		}
		if i.ConfigRevision > 0 {
			current, err := readActiveConfig(ctx, tx)
			if err != nil {
				return err
			}
			if subtle.ConstantTimeCompare([]byte(p.KeyConfirmation), []byte(current.Snapshot.KeyConfirmation)) != 1 {
				return ports.ErrConflict
			}
			if p.Revision == i.ConfigRevision {
				if !domain.SameProbeConfigMetadata(p.ProbeConfigMetadata, current.Snapshot.ProbeConfigMetadata) || !reflect.DeepEqual(assignments, current.Assignments) {
					return ports.ErrConflict
				}
				return nil
			}
		}
		row := configRow{Revision: p.Revision, HubID: p.HubID, ProbeID: p.ProbeID, SchemaVersion: p.SchemaVersion, SHA256: p.SHA256, CreatedAt: p.CreatedAt.UTC().UnixMicro(), EffectiveAt: p.EffectiveAt.UTC().UnixMicro(), AppliedAt: config.AppliedAt.UTC().UnixMicro(), KeyConfirmation: p.KeyConfirmation, ProtectedPayload: p.ProtectedPayload}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		for _, a := range assignments {
			var old struct {
				Generation int64
				Revision   int64
			}
			err := tx.NewRaw("SELECT generation, revision FROM edge_assignments WHERE monitor_id = ?", a.MonitorID).Scan(ctx, &old)
			if err != nil && !errors.Is(storageError(ctx, err), ports.ErrNotFound) {
				return err
			}
			if err == nil && (a.Generation < old.Generation || old.Revision != i.ConfigRevision && a.Generation == old.Generation) {
				return ports.ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO edge_assignments (monitor_id, generation, active, revision) VALUES (?, ?, ?, ?)
			 ON CONFLICT (monitor_id) DO UPDATE SET generation = excluded.generation, active = excluded.active, revision = excluded.revision`, a.MonitorID, a.Generation, a.Active, p.Revision); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE edge_identity SET config_revision = ? WHERE id = 1", p.Revision)
		return err
	})
}

// ReadActiveConfig captures exact protected bytes and scheduler IDs in one read.
func (s *Store) ReadActiveConfig(ctx context.Context) (domain.EdgeActiveConfig, error) {
	var result domain.EdgeActiveConfig
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var err error
		result, err = readActiveConfig(ctx, tx)
		return err
	})
	if err != nil {
		return domain.EdgeActiveConfig{}, storageError(ctx, err)
	}
	return result, nil
}

func readActiveConfig(ctx context.Context, db bun.IDB) (domain.EdgeActiveConfig, error) {
	var row configRow
	if err := db.NewSelect().Model(&row).Join("JOIN edge_identity i ON i.id = 1 AND i.config_revision = c.revision AND i.hub_id = c.hub_id AND i.probe_id = c.probe_id").Scan(ctx); err != nil {
		return domain.EdgeActiveConfig{}, storageError(ctx, err)
	}
	assignments := make([]domain.EdgeAssignmentIdentity, 0)
	if err := db.NewRaw("SELECT monitor_id, generation, active FROM edge_assignments WHERE revision = ? ORDER BY monitor_id", row.Revision).Scan(ctx, &assignments); err != nil {
		return domain.EdgeActiveConfig{}, storageError(ctx, err)
	}
	return row.active(assignments), nil
}
