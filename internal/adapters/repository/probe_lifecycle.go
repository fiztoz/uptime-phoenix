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

type probeRevocationRow struct {
	bun.BaseModel `bun:"table:probe_revocations"`
	OperationID   string `bun:"operation_id,pk"`
	ProbeID       string
	CreatedAt     time.Time
}

func (r probeRevocationRow) operation() *domain.ProbeOperation {
	return &domain.ProbeOperation{OperationID: r.OperationID, ProbeID: r.ProbeID,
		Kind: domain.ProbeOperationRevoke, Status: domain.ProbeOperationSucceeded,
		Phase: "revoked_locally", CreatedAt: r.CreatedAt.UTC(), UpdatedAt: r.CreatedAt.UTC()}
}

// ProbeLifecycleStore fences a revoked identity and retains its receipt/history.
type ProbeLifecycleStore struct{ db *bun.DB }

// NewProbeLifecycleStore supports both hub engines.
func NewProbeLifecycleStore(db *bun.DB) *ProbeLifecycleStore { return &ProbeLifecycleStore{db: db} }

var _ ports.ProbeLifecycleRepository = (*ProbeLifecycleStore)(nil)

// RevokeProbe atomically commits the receipt, disables admission, and invalidates
// existing owners. Retries return the original receipt. Remote execution remains
// explicitly unconfirmed: a partitioned source can continue its accepted work.
func (s *ProbeLifecycleStore) RevokeProbe(ctx context.Context, probeID, operationID string, at time.Time) (*domain.ProbeOperation, error) {
	return s.revoke(ctx, probeID, operationID, at, false)
}

// DeleteProbe soft-deletes only an unassigned remote identity. Tombstones and
// observations survive, and the stable key cannot be reused for another source.
func (s *ProbeLifecycleStore) DeleteProbe(ctx context.Context, probeID, operationID string, at time.Time) error {
	_, err := s.revoke(ctx, probeID, operationID, at, true)
	return err
}

func (s *ProbeLifecycleStore) revoke(ctx context.Context, probeID, operationID string, at time.Time, deleting bool) (*domain.ProbeOperation, error) {
	if s == nil || s.db == nil || !validRemoteProbeID(probeID) || !domain.ValidProbeOperationID(operationID) || at.IsZero() {
		return nil, domain.ErrValidation
	}
	at = at.UTC().Truncate(time.Microsecond)
	var result *domain.ProbeOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		// The same lock precedes assignment, session, replay and command writes.
		if _, err := tx.NewUpdate().Table("probes").Set("id = id").Where("id = ?", probeID).Exec(ctx); err != nil {
			return err
		}
		var registration probeRegistrationModel
		if err := tx.NewSelect().Model(&registration).Where("id = ?", probeID).Scan(ctx); err != nil {
			return err
		}
		if registration.Kind != domain.ProbeKindRemote {
			return domain.ErrValidation
		}
		if deleting {
			assigned, err := tx.NewSelect().Table("monitor_probe_assignments").Where("probe_id = ? AND active = ?", probeID, true).Exists(ctx)
			if err != nil {
				return err
			}
			if assigned {
				return ports.ErrConflict
			}
		}
		var receipt probeRevocationRow
		err := tx.NewSelect().Model(&receipt).Where("probe_id = ?", probeID).Scan(ctx)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if errors.Is(err, sql.ErrNoRows) {
			receipt = probeRevocationRow{OperationID: operationID, ProbeID: probeID, CreatedAt: at}
			if _, err := tx.NewInsert().Model(&receipt).Exec(ctx); err != nil {
				return err
			}
		}
		if registration.RevokedAt == nil || deleting && registration.DeletedAt == nil {
			if registration.Revision == math.MaxInt64 {
				return ports.ErrConflict
			}
			q := tx.NewUpdate().Table("probes").Set("enabled = ?", false).
				Set("revoked_at = ?", receipt.CreatedAt.UTC()).Set("revision = revision + 1").Set("updated_at = ?", at).
				Where("id = ?", probeID)
			if deleting {
				q = q.Set("deleted_at = ?", at)
			}
			if _, err := q.Exec(ctx); err != nil {
				return err
			}
		}
		if _, err := tx.NewUpdate().Table("probe_runtime_owners").Set("owner_id = ''").Set("lease_until = 0").Where("probe_id = ?", probeID).Exec(ctx); err != nil {
			return err
		}
		if err := invalidateRuntimeConnector(ctx, tx, probeID); err != nil {
			return err
		}
		result = receipt.operation()
		return nil
	})
	return result, probeRegistryError(err)
}

// GetRevocation returns a durable receipt even after its probe is soft-deleted.
func (s *ProbeLifecycleStore) GetRevocation(ctx context.Context, operationID string) (*domain.ProbeOperation, error) {
	if !domain.ValidProbeOperationID(operationID) {
		return nil, domain.ErrValidation
	}
	var row probeRevocationRow
	if err := s.db.NewSelect().Model(&row).Where("operation_id = ?", operationID).Scan(ctx); err != nil {
		return nil, probeRegistryError(err)
	}
	return row.operation(), nil
}
