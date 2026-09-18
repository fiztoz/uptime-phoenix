package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeConfigModel struct {
	bun.BaseModel    `bun:"table:probe_config_snapshots,alias:config"`
	ProbeID          string    `bun:"probe_id,pk"`
	Revision         int64     `bun:"revision,pk"`
	HubID            string    `bun:"hub_id,notnull"`
	SchemaVersion    int       `bun:"schema_version,notnull"`
	SHA256           string    `bun:"sha256,notnull"`
	CreatedAt        time.Time `bun:"source_created_at,notnull"`
	EffectiveAt      time.Time `bun:"effective_at,notnull"`
	ProtectedPayload []byte    `bun:"protected_payload,notnull"`
	StoredAt         time.Time `bun:"stored_at,notnull"`
}

func (m probeConfigModel) domain() *domain.ProtectedProbeConfig {
	return &domain.ProtectedProbeConfig{ProbeConfigMetadata: domain.NormalizeProbeConfigMetadata(domain.ProbeConfigMetadata{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: m.HubID, ProbeID: m.ProbeID}, Revision: m.Revision,
		SchemaVersion: m.SchemaVersion, SHA256: m.SHA256, CreatedAt: m.CreatedAt, EffectiveAt: m.EffectiveAt}),
		ProtectedPayload: bytes.Clone(m.ProtectedPayload), StoredAt: m.StoredAt.UTC()}
}

// ProbeConfigStore retains prepared ciphertext, never active runtime configuration.
type ProbeConfigStore struct{ db *bun.DB }

// NewProbeConfigStore constructs the shared SQL implementation.
func NewProbeConfigStore(db *bun.DB) *ProbeConfigStore { return &ProbeConfigStore{db: db} }

var _ ports.ProbeConfigRepository = (*ProbeConfigStore)(nil)

// Save atomically compares the latest revision and inserts immutable content.
// The registration row serializes writers even before a first snapshot exists.
// It conveys storage identity only, not authentication or activation authority.
func (r *ProbeConfigStore) Save(ctx context.Context, snapshot domain.ProtectedProbeConfig, expectedRevision int64) (*domain.ProtectedProbeConfig, error) {
	m := domain.NormalizeProbeConfigMetadata(snapshot.ProbeConfigMetadata)
	if !domain.ValidProbeConfigMetadata(m) || expectedRevision < 0 || len(snapshot.ProtectedPayload) <= domain.ProbeConfigProtectionOverhead ||
		len(snapshot.ProtectedPayload) > domain.MaxProbeConfigBytes+domain.ProbeConfigProtectionOverhead || snapshot.ProtectedPayload[0] != 1 {
		return nil, domain.ErrValidation
	}
	row := probeConfigModel{ProbeID: m.ProbeID, HubID: m.HubID, Revision: m.Revision, SchemaVersion: m.SchemaVersion,
		SHA256: m.SHA256, CreatedAt: m.CreatedAt, EffectiveAt: m.EffectiveAt, ProtectedPayload: bytes.Clone(snapshot.ProtectedPayload),
		StoredAt: time.Now().UTC().Truncate(time.Microsecond)}
	var result *domain.ProtectedProbeConfig
	err := r.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if tx.Dialect().Name() == dialect.SQLite {
			if _, err := tx.NewUpdate().Table("probes").Set("id = id").Where("id = ?", m.ProbeID).Exec(ctx); err != nil {
				return err
			}
		}
		var id string
		lock := tx.NewSelect().Table("probes").Column("id").Where("id = ?", m.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			lock = lock.For("UPDATE")
		}
		if err := lock.Scan(ctx, &id); err != nil {
			return probeRegistryError(err)
		}
		latest := new(probeConfigModel)
		q := tx.NewSelect().Model(latest).Where("probe_id = ?", m.ProbeID).Order("revision DESC").Limit(1)
		// This is the transaction's first consistent read, after the probe lock.
		// No snapshot-index gap lock is needed for independently prepared probes.
		err := q.Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if latest.HubID != m.HubID {
				return ports.ErrConflict
			}
			if latest.Revision == m.Revision {
				if !domain.SameProbeConfigMetadata(latest.domain().ProbeConfigMetadata, m) {
					return ports.ErrConflict
				}
				result = latest.domain()
				return nil
			}
		}
		if latest.Revision != expectedRevision || m.Revision <= latest.Revision {
			return ports.ErrConflict
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return probeRegistryError(err)
		}
		result = row.domain()
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("save prepared snapshot: %w", probeRegistryError(err))
	}
	return result, nil
}

// Get reads an exact retained revision; it never substitutes newer credentials.
func (r *ProbeConfigStore) Get(ctx context.Context, probeID string, revision int64) (*domain.ProtectedProbeConfig, error) {
	if !validConfigProbe(probeID) || revision < 1 {
		return nil, domain.ErrValidation
	}
	m := new(probeConfigModel)
	if err := r.db.NewSelect().Model(m).Where("probe_id = ? AND revision = ?", probeID, revision).Scan(ctx); err != nil {
		return nil, probeRegistryError(err)
	}
	return m.domain(), nil
}

// Latest reads the greatest prepared revision. It does not mean applied or active.
func (r *ProbeConfigStore) Latest(ctx context.Context, probeID string) (*domain.ProtectedProbeConfig, error) {
	if !validConfigProbe(probeID) {
		return nil, domain.ErrValidation
	}
	m := new(probeConfigModel)
	if err := r.db.NewSelect().Model(m).Where("probe_id = ?", probeID).Order("revision DESC").Limit(1).Scan(ctx); err != nil {
		return nil, probeRegistryError(err)
	}
	return m.domain(), nil
}

func validConfigProbe(probeID string) bool {
	if probeID == domain.LocalProbeID {
		return true
	}
	id, err := canonicalIdentity("probe_id", probeID)
	return err == nil && id == probeID
}
