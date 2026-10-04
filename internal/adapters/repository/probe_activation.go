package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type probeActiveConfigModel struct {
	bun.BaseModel   `bun:"table:probe_active_configs,alias:active"`
	ProbeID         string    `bun:"probe_id,pk"`
	Revision        int64     `bun:"revision,notnull"`
	SHA256          string    `bun:"sha256,notnull"`
	HubID           string    `bun:"hub_id,notnull"`
	AppliedAt       time.Time `bun:"applied_at,notnull"`
	AssignmentCount int       `bun:"assignment_count,notnull"`
}

type probeConfigAppliedReceiptModel struct {
	bun.BaseModel   `bun:"table:probe_config_applied_receipts,alias:receipt"`
	ProbeID         string    `bun:"probe_id,pk"`
	Revision        int64     `bun:"revision,pk"`
	SHA256          string    `bun:"sha256,notnull"`
	HubID           string    `bun:"hub_id,notnull"`
	AppliedAt       time.Time `bun:"applied_at,notnull"`
	AssignmentCount int       `bun:"assignment_count,notnull"`
	CreatedAt       time.Time `bun:"created_at,notnull"`
}

// ProbeActivationStore manages active configuration pointers and applied receipts.
type ProbeActivationStore struct {
	db      *bun.DB
	encoder ports.LocalProbeConfigEncoder
}

// NewProbeActivationStore constructs a store instance for MariaDB or SQLite.
func NewProbeActivationStore(db *bun.DB, encoder ports.LocalProbeConfigEncoder) *ProbeActivationStore {
	return &ProbeActivationStore{db: db, encoder: encoder}
}

var _ ports.ProbeConfigActivationRepository = (*ProbeActivationStore)(nil)

// GetActive returns the current active configuration pointer for probeID.
func (r *ProbeActivationStore) GetActive(ctx context.Context, probeID string) (*domain.ProbeActiveConfig, error) {
	if r == nil || r.db == nil || !validConfigProbe(probeID) {
		return nil, domain.ErrValidation
	}
	m := new(probeActiveConfigModel)
	if err := r.db.NewSelect().Model(m).Where("probe_id = ?", probeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err
	}
	return &domain.ProbeActiveConfig{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: m.HubID, ProbeID: m.ProbeID},
		Revision:          m.Revision,
		SHA256:            m.SHA256,
		AppliedAt:         m.AppliedAt.UTC(),
		AssignmentCount:   m.AssignmentCount,
	}, nil
}

// GetReceipt returns the durable receipt for probeID and revision.
func (r *ProbeActivationStore) GetReceipt(ctx context.Context, probeID string, revision int64) (*domain.ProbeActiveConfig, error) {
	if r == nil || r.db == nil || !validConfigProbe(probeID) || revision <= 0 {
		return nil, domain.ErrValidation
	}
	m := new(probeConfigAppliedReceiptModel)
	if err := r.db.NewSelect().Model(m).Where("probe_id = ? AND revision = ?", probeID, revision).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ports.ErrNotFound
		}
		return nil, err
	}
	return &domain.ProbeActiveConfig{
		ProbeConfigTarget: domain.ProbeConfigTarget{HubID: m.HubID, ProbeID: m.ProbeID},
		Revision:          m.Revision,
		SHA256:            m.SHA256,
		AppliedAt:         m.AppliedAt.UTC(),
		AssignmentCount:   m.AssignmentCount,
	}, nil
}

// ActivateLocal atomically verifies source freshness, checks expectedActiveRevision,
// verifies trusted installation authority, checks candidate snapshot hash,
// and commits the active pointer and receipt together.
func (r *ProbeActivationStore) ActivateLocal(ctx context.Context, params ports.LocalActivationParams) (*domain.ProbeActiveConfig, error) {
	if r == nil || r.db == nil || r.encoder == nil ||
		!domain.ValidProbeConfigTarget(params.Target) || params.Target.ProbeID != domain.LocalProbeID ||
		params.Revision <= 0 || !domain.ValidKeyHash(params.SHA256) || params.ExpectedActiveRevision < 0 {
		return nil, domain.ErrValidation
	}

	var applied *domain.ProbeActiveConfig
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		// 1. Lock & verify probe registration.
		if tx.Dialect().Name() == dialect.SQLite {
			if _, err := tx.NewUpdate().Table("probes").Set("id = id").Where("id = ?", params.Target.ProbeID).Exec(ctx); err != nil {
				return err
			}
		}
		probeLock := tx.NewSelect().Table("probes").Column("id").Where("id = ?", params.Target.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			probeLock = probeLock.For("UPDATE")
		}
		var probeID string
		if err := probeLock.Scan(ctx, &probeID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return probeRegistryError(err)
		}

		var probe probeRegistrationModel
		if err := tx.NewSelect().Model(&probe).Where("id = ?", params.Target.ProbeID).Scan(ctx); err != nil {
			return probeRegistryError(err)
		}
		if !probe.Enabled || probe.Kind != domain.ProbeKindLocal {
			return domain.ErrValidation
		}

		// 2. Lock & verify installation authority.
		instLock := tx.NewSelect().Model((*probeInstallationModel)(nil)).Where("id = 1")
		if tx.Dialect().Name() == dialect.MySQL {
			instLock = instLock.For("UPDATE")
		}
		var inst probeInstallationModel
		if err := instLock.Scan(ctx, &inst); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.ErrValidation
			}
			return err
		}
		if inst.HubID != params.Target.HubID {
			return ports.ErrConflict
		}

		// 3. Check current active configuration.
		activeLock := tx.NewSelect().Model((*probeActiveConfigModel)(nil)).Where("probe_id = ?", params.Target.ProbeID)
		if tx.Dialect().Name() == dialect.MySQL {
			activeLock = activeLock.For("UPDATE")
		}
		var active probeActiveConfigModel
		errActive := activeLock.Scan(ctx, &active)
		if errActive != nil && !errors.Is(errActive, sql.ErrNoRows) {
			return errActive
		}
		hasActive := errActive == nil

		if hasActive {
			// Older revision is rejected.
			if active.Revision > params.Revision {
				return ports.ErrConflict
			}
			// Same revision check.
			if active.Revision == params.Revision {
				if active.SHA256 == params.SHA256 {
					// Same-revision/same-hash retry: return durable prior result idempotently.
					applied = &domain.ProbeActiveConfig{
						ProbeConfigTarget: params.Target,
						Revision:          active.Revision,
						SHA256:            active.SHA256,
						AppliedAt:         active.AppliedAt.UTC(),
						AssignmentCount:   active.AssignmentCount,
					}
					return nil
				}
				// Same revision with different hash conflicts.
				return ports.ErrConflict
			}
			// Expected active revision check (fencing).
			if active.Revision != params.ExpectedActiveRevision {
				return ports.ErrConflict
			}
		} else {
			// No active config yet: ExpectedActiveRevision must be 0.
			if params.ExpectedActiveRevision != 0 {
				return ports.ErrConflict
			}
		}

		// 4. Verify candidate snapshot exists and matches target/hash.
		var snapshot probeConfigModel
		if err := tx.NewSelect().Model(&snapshot).
			Where("probe_id = ? AND revision = ?", params.Target.ProbeID, params.Revision).Scan(ctx); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ports.ErrNotFound
			}
			return probeRegistryError(err)
		}
		if snapshot.HubID != params.Target.HubID || snapshot.SHA256 != params.SHA256 {
			return ports.ErrConflict
		}

		// 5. Step B: Source Freshness Enforcement.
		source, err := readLocalConfigSource(ctx, tx)
		if err != nil {
			return err
		}
		definition, err := services.ResolveLocalProbeConfig(source)
		if err != nil {
			return err
		}
		definition.Target = params.Target
		definition.Revision = params.Revision
		definition.CreatedAt = snapshot.CreatedAt.UTC()
		definition.EffectiveAt = snapshot.EffectiveAt.UTC()

		doc, err := r.encoder.EncodeLocal(definition)
		if err != nil {
			return err
		}
		currentSHA256 := fmt.Sprintf("%x", sha256.Sum256(doc))
		if currentSHA256 != params.SHA256 {
			return ports.ErrConflict
		}

		assignmentCount := len(definition.Assignments)
		if params.AssignmentCount >= 0 && params.AssignmentCount != assignmentCount {
			return ports.ErrConflict
		}

		// 6. Persist active pointer and applied receipt together.
		now := params.AppliedAt.UTC().Truncate(time.Microsecond)
		activeModel := probeActiveConfigModel{
			ProbeID:         params.Target.ProbeID,
			Revision:        params.Revision,
			SHA256:          params.SHA256,
			HubID:           params.Target.HubID,
			AppliedAt:       now,
			AssignmentCount: assignmentCount,
		}
		receiptModel := probeConfigAppliedReceiptModel{
			ProbeID:         params.Target.ProbeID,
			Revision:        params.Revision,
			SHA256:          params.SHA256,
			HubID:           params.Target.HubID,
			AppliedAt:       now,
			AssignmentCount: assignmentCount,
			CreatedAt:       time.Now().UTC().Truncate(time.Microsecond),
		}

		if hasActive {
			if _, err := tx.NewUpdate().Model(&activeModel).WherePK().Exec(ctx); err != nil {
				return err
			}
		} else {
			if _, err := tx.NewInsert().Model(&activeModel).Exec(ctx); err != nil {
				return err
			}
		}

		if _, err := tx.NewInsert().Model(&receiptModel).Exec(ctx); err != nil {
			return err
		}

		applied = &domain.ProbeActiveConfig{
			ProbeConfigTarget: params.Target,
			Revision:          params.Revision,
			SHA256:            params.SHA256,
			AppliedAt:         now,
			AssignmentCount:   assignmentCount,
		}
		return nil
	})
	if err != nil {
		return nil, probeRegistryError(err)
	}
	return applied, nil
}

// configAuthorityTxOptions makes ordinary source SELECTs locking reads on MariaDB,
// including predicate/gap locks for relationship insertion. Never inherit READ COMMITTED.
func configAuthorityTxOptions(db *bun.DB) *sql.TxOptions {
	if db.Dialect().Name() == dialect.MySQL {
		return &sql.TxOptions{Isolation: sql.LevelSerializable}
	}
	return nil
}

// ReadAppliedLocal captures execution settings only when the entire source graph
// still encodes to the selected immutable snapshot. Provider/checker I/O follows commit.
func (r *ProbeActivationStore) ReadAppliedLocal(ctx context.Context) (*domain.LocalProbeConfigDefinition, error) {
	var definition *domain.LocalProbeConfigDefinition
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		var err error
		definition, err = readAppliedLocalTx(ctx, tx, r.encoder)
		return err
	})
	if err != nil {
		if errors.Is(err, ports.ErrConflict) || errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrInternal // Source decoder errors can contain credentials.
	}
	return definition, nil
}

func readAppliedLocalTx(ctx context.Context, tx bun.Tx, encoder ports.LocalProbeConfigEncoder) (*domain.LocalProbeConfigDefinition, error) {
	if tx.Dialect().Name() == dialect.SQLite {
		if _, err := tx.NewUpdate().Table("probes").Set("id = id").Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
			return nil, err
		}
	}
	var active probeActiveConfigModel
	if err := tx.NewSelect().Model(&active).Where("probe_id = ?", domain.LocalProbeID).Scan(ctx); err != nil {
		return nil, probeRegistryError(err)
	}
	var snapshot probeConfigModel
	if err := tx.NewSelect().Model(&snapshot).Where("probe_id = ? AND revision = ?", domain.LocalProbeID, active.Revision).Scan(ctx); err != nil {
		return nil, err
	}
	if active.HubID != snapshot.HubID || active.SHA256 != snapshot.SHA256 {
		return nil, ports.ErrConflict
	}
	source, err := readLocalConfigSource(ctx, tx)
	if err != nil {
		return nil, err
	}
	resolved, err := services.ResolveLocalProbeConfig(source)
	if err != nil {
		return nil, err
	}
	resolved.Target = domain.ProbeConfigTarget{HubID: active.HubID, ProbeID: domain.LocalProbeID}
	resolved.Revision = active.Revision
	resolved.CreatedAt = snapshot.CreatedAt.UTC()
	resolved.EffectiveAt = snapshot.EffectiveAt.UTC()
	document, err := encoder.EncodeLocal(resolved)
	if err != nil {
		return nil, err
	}
	if fmt.Sprintf("%x", sha256.Sum256(document)) != active.SHA256 {
		return nil, ports.ErrConflict
	}
	return &resolved, nil
}

// runConfigAuthorityTx retries database serialization failures from concurrent
// writers. The callback contains database work only; it never calls a provider.
func runConfigAuthorityTx(ctx context.Context, db *bun.DB, fn func(context.Context, bun.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := db.RunInTx(ctx, configAuthorityTxOptions(db), fn)
		var conflict *mysql.MySQLError
		if attempt >= 3 || ctx.Err() != nil || !errors.As(err, &conflict) || (conflict.Number != 1020 && conflict.Number != 1213) {
			return err
		}
	}
}
