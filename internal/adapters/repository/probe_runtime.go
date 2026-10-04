package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeRuntimeModel struct {
	bun.BaseModel `bun:"table:probe_runtime_owners,alias:pro"`
	ProbeID       string `bun:",pk"`
	OwnerID       string
	Epoch         int64
	LeaseUntil    int64
}

var _ ports.ProbeRuntimeLeaseRepository = (*ProbeConnectorStore)(nil)

func (r probeRuntimeModel) lease() domain.ProbeRuntimeLease {
	return domain.ProbeRuntimeLease{ProbeID: r.ProbeID, OwnerID: r.OwnerID, Epoch: r.Epoch, LeaseUntil: time.Unix(r.LeaseUntil, 0).UTC()}
}

func readProbeRuntime(ctx context.Context, tx bun.Tx, probeID string) (probeRuntimeModel, error) {
	var row probeRuntimeModel
	q := tx.NewSelect().Model(&row).Where("probe_id = ?", probeID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	err := q.Scan(ctx)
	return row, err
}

func matchesProbeRuntime(row probeRuntimeModel, lease domain.ProbeRuntimeLease) bool {
	return lease.Epoch > 0 && row.Epoch == lease.Epoch && row.OwnerID == lease.OwnerID
}

func invalidateRuntimeConnector(ctx context.Context, tx bun.Tx, probeID string) error {
	_, err := tx.ExecContext(ctx, "UPDATE probe_sessions SET owner_id = '', lease_until = 0, connected = ? WHERE probe_id = ?", false, probeID)
	return err
}

// AcquireRuntime exclusively owns reconnect attempts until release or expiry.
// Even the same owner cannot reacquire a live epoch: duplicate local loops must
// not reset watchdog state. Legacy sessions drain before first adoption.
func (s *ProbeConnectorStore) AcquireRuntime(ctx context.Context, probeID, ownerID string) (domain.ProbeRuntimeLease, error) {
	if !domain.ValidHubID(ownerID) {
		return domain.ProbeRuntimeLease{}, domain.ErrValidation
	}
	var result domain.ProbeRuntimeLease
	err := s.transaction(ctx, probeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		if !enabled {
			return ports.ErrConflict
		}
		row, err := readProbeRuntime(ctx, tx, probeID)
		missing := errors.Is(err, sql.ErrNoRows)
		if err != nil && !missing {
			return err
		}
		if !missing && (row.LeaseUntil > now || row.Epoch == math.MaxInt64) {
			return ports.ErrConflict
		}
		if missing {
			child, err := readProbeSession(ctx, tx, probeID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if child.LeaseUntil > now {
				return ports.ErrConflict
			}
		}
		row = probeRuntimeModel{ProbeID: probeID, OwnerID: ownerID, Epoch: row.Epoch + 1, LeaseUntil: now + 60}
		if missing {
			_, err = tx.NewInsert().Model(&row).Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		}
		if err != nil {
			return err
		}
		if err := invalidateRuntimeConnector(ctx, tx, probeID); err != nil {
			return err
		}
		result = row.lease()
		return nil
	})
	if err != nil {
		return domain.ProbeRuntimeLease{}, err
	}
	return result, nil
}

// RenewRuntime preserves the epoch only while the registration and lease live.
func (s *ProbeConnectorStore) RenewRuntime(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeRuntimeLease, error) {
	var result domain.ProbeRuntimeLease
	err := s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		row, err := readProbeRuntime(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !enabled || !matchesProbeRuntime(row, lease) || row.LeaseUntil <= now {
			return ports.ErrConflict
		}
		// A backward DB clock step must not shorten parent authority below a
		// child deadline already committed under this epoch.
		row.LeaseUntil = max(row.LeaseUntil, now+60)
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		result = row.lease()
		return nil
	})
	if err != nil {
		return domain.ProbeRuntimeLease{}, err
	}
	return result, nil
}

// ReleaseRuntime atomically invalidates its session without discarding either
// epoch or generation. A stale release can never cancel a replacement owner.
func (s *ProbeConnectorStore) ReleaseRuntime(ctx context.Context, lease domain.ProbeRuntimeLease) error {
	return s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, _ bool, _ int64) error {
		row, err := readProbeRuntime(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !matchesProbeRuntime(row, lease) {
			return ports.ErrConflict
		}
		row.OwnerID, row.LeaseUntil = "", 0
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		return invalidateRuntimeConnector(ctx, tx, lease.ProbeID)
	})
}

// AcquireRuntimeConnector advances the connection generation under the stable
// runtime epoch. Session authority expires no later than its parent's DB lease.
func (s *ProbeConnectorStore) AcquireRuntimeConnector(ctx context.Context, lease domain.ProbeRuntimeLease) (domain.ProbeConnectorLease, error) {
	var result domain.ProbeConnectorLease
	err := s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		row, err := readProbeRuntime(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !enabled || !matchesProbeRuntime(row, lease) || row.LeaseUntil <= now {
			return ports.ErrConflict
		}
		result, err = acquireProbeConnector(ctx, tx, lease.ProbeID, lease.OwnerID, now, row.LeaseUntil)
		return err
	})
	if err != nil {
		return domain.ProbeConnectorLease{}, err
	}
	return result, nil
}

// connectorLeaseDeadline also supports an unmigrated legacy session. Once a
// runtime row exists it is mandatory authority, including after release.
func connectorLeaseDeadline(ctx context.Context, tx bun.Tx, session probeSessionModel, now int64) (int64, error) {
	parent, err := readProbeRuntime(ctx, tx, session.ProbeID)
	if errors.Is(err, sql.ErrNoRows) {
		return now + 60, nil
	}
	if err != nil {
		return 0, err
	}
	if parent.OwnerID != session.OwnerID || parent.LeaseUntil <= now {
		return 0, ports.ErrConflict
	}
	return min(now+60, parent.LeaseUntil), nil
}
