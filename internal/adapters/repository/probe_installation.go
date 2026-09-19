package repository

import (
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

type probeInstallationModel struct {
	bun.BaseModel  `bun:"table:probe_installation,alias:pi"`
	ID             int       `bun:"id,pk"`
	HubID          string    `bun:"hub_id,notnull"`
	KeyHash        string    `bun:"key_hash,notnull"`
	ProtocolFloor  int       `bun:"protocol_floor,notnull"`
	AuthorityEpoch int64     `bun:"authority_epoch,notnull"`
	CreatedAt      time.Time `bun:"created_at,notnull"`
	UpdatedAt      time.Time `bun:"updated_at,notnull"`
}

func (m probeInstallationModel) domain() *domain.ProbeInstallation {
	return &domain.ProbeInstallation{
		HubID:          m.HubID,
		KeyHash:        m.KeyHash,
		ProtocolFloor:  m.ProtocolFloor,
		AuthorityEpoch: m.AuthorityEpoch,
		CreatedAt:      m.CreatedAt.UTC().Truncate(time.Microsecond),
		UpdatedAt:      m.UpdatedAt.UTC().Truncate(time.Microsecond),
	}
}

// ProbeInstallationStore manages singleton installation identity and key confirmation.
type ProbeInstallationStore struct {
	db *bun.DB
}

// NewProbeInstallationStore creates a store instance backed by Bun.
func NewProbeInstallationStore(db *bun.DB) *ProbeInstallationStore {
	return &ProbeInstallationStore{db: db}
}

var _ ports.ProbeInstallationRepository = (*ProbeInstallationStore)(nil)

// Get returns the singleton installation record, or ports.ErrNotFound if not yet initialized.
func (s *ProbeInstallationStore) Get(ctx context.Context) (*domain.ProbeInstallation, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrValidation
	}
	row := new(probeInstallationModel)
	if err := s.db.NewSelect().Model(row).Where("id = 1").Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, fmt.Errorf("read probe installation: %w", err)
	}
	return row.domain(), nil
}

// Initialize atomically inserts the singleton installation record.
// If an identical record already exists, it returns the existing record idempotently.
// If a different record exists, it returns ports.ErrConflict.
func (s *ProbeInstallationStore) Initialize(ctx context.Context, inst domain.ProbeInstallation) (*domain.ProbeInstallation, error) {
	if s == nil || s.db == nil {
		return nil, domain.ErrValidation
	}
	normalized := domain.NormalizeProbeInstallation(inst)
	if !domain.ValidProbeInstallation(normalized) {
		return nil, domain.ErrValidation
	}

	var opts *sql.TxOptions
	if s.db.Dialect().Name() == dialect.MySQL {
		opts = &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
	}

	var result *domain.ProbeInstallation
	err := s.db.RunInTx(ctx, opts, func(ctx context.Context, tx bun.Tx) error {
		existing := new(probeInstallationModel)
		q := tx.NewSelect().Model(existing).Where("id = 1")
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		err := q.Scan(ctx)
		if err == nil {
			if domain.SameProbeInstallation(*existing.domain(), normalized) {
				result = existing.domain()
				return nil
			}
			return ports.ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}

		row := probeInstallationModel{
			ID:             1,
			HubID:          normalized.HubID,
			KeyHash:        normalized.KeyHash,
			ProtocolFloor:  normalized.ProtocolFloor,
			AuthorityEpoch: normalized.AuthorityEpoch,
			CreatedAt:      normalized.CreatedAt,
			UpdatedAt:      normalized.UpdatedAt,
		}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			// On concurrent race on an empty table, a duplicate key may occur.
			// Re-read existing under lock.
			retry := new(probeInstallationModel)
			rq := tx.NewSelect().Model(retry).Where("id = 1")
			if tx.Dialect().Name() == dialect.MySQL {
				rq = rq.For("UPDATE")
			}
			if rerr := rq.Scan(ctx); rerr == nil {
				if domain.SameProbeInstallation(*retry.domain(), normalized) {
					result = retry.domain()
					return nil
				}
				return ports.ErrConflict
			}
			return fmt.Errorf("insert probe installation: %w", err)
		}
		result = row.domain()
		return nil
	})
	if err != nil {
		if errors.Is(err, ports.ErrConflict) {
			return nil, ports.ErrConflict
		}
		return nil, fmt.Errorf("initialize probe installation: %w", err)
	}
	return result, nil
}

// VerifyRetainedSnapshots iterates through all retained probe configuration snapshots in bounded batches,
// verifying that each snapshot's hub_id matches the trusted hubID and invoking check on its metadata and ciphertext.
func (s *ProbeInstallationStore) VerifyRetainedSnapshots(ctx context.Context, hubID string, check func(metadata domain.ProbeConfigMetadata, payload []byte) error) error {
	if s == nil || s.db == nil || !domain.ValidHubID(hubID) || check == nil {
		return domain.ErrValidation
	}

	const batchSize = 20
	var lastProbeID string
	var lastRevision int64

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		var batch []probeConfigModel
		q := s.db.NewSelect().Model(&batch).
			Order("probe_id ASC", "revision ASC").
			Limit(batchSize)
		if lastProbeID != "" {
			q = q.Where("probe_id > ? OR (probe_id = ? AND revision > ?)", lastProbeID, lastProbeID, lastRevision)
		}

		if err := q.Scan(ctx); err != nil {
			return fmt.Errorf("scan retained snapshots: %w", err)
		}

		if len(batch) == 0 {
			break
		}

		for _, row := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			if row.HubID != hubID {
				return domain.ErrProbeInstallationConflict
			}
			snap := row.domain()
			if err := check(snap.ProbeConfigMetadata, snap.ProtectedPayload); err != nil {
				return err
			}
			lastProbeID = row.ProbeID
			lastRevision = row.Revision
		}

		if len(batch) < batchSize {
			break
		}
	}

	return nil
}

// HasSnapshots returns true if any prepared probe configuration snapshots exist.
func (s *ProbeInstallationStore) HasSnapshots(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, domain.ErrValidation
	}
	return s.db.NewSelect().Table("probe_config_snapshots").Exists(ctx)
}
