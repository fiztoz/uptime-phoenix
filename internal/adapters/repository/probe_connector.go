package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeSessionModel struct {
	bun.BaseModel `bun:"table:probe_sessions,alias:ps"`
	ProbeID       string `bun:",pk"`
	OwnerID       string
	Generation    int64
	LeaseUntil    int64
	Connected     bool
}

func (m probeSessionModel) lease() domain.ProbeConnectorLease {
	return domain.ProbeConnectorLease{ProbeID: m.ProbeID, OwnerID: m.OwnerID, Generation: m.Generation, LeaseUntil: time.Unix(m.LeaseUntil, 0).UTC(), Connected: m.Connected}
}

// ProbeConnectorStore serializes connection ownership using the hub DB clock.
type ProbeConnectorStore struct{ db *bun.DB }

var _ ports.ProbeConnectorLeaseRepository = (*ProbeConnectorStore)(nil)

// NewProbeConnectorStore supports either hub SQL adapter.
func NewProbeConnectorStore(db *bun.DB) *ProbeConnectorStore { return &ProbeConnectorStore{db: db} }

func (s *ProbeConnectorStore) transaction(ctx context.Context, probeID string, action func(context.Context, bun.Tx, bool, int64) error) error {
	if !validRemoteProbeID(probeID) {
		return domain.ErrValidation
	}
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Same probe-before-dependent-rows lock order as registry/configuration.
		if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", probeID); err != nil {
			return err
		}
		var probe probeRegistrationModel
		q := tx.NewSelect().Model(&probe).Where("id = ?", probeID)
		if tx.Dialect().Name() == dialect.MySQL {
			q = q.For("UPDATE")
		}
		if err := q.Scan(ctx); err != nil {
			return err
		}
		if probe.Kind != domain.ProbeKindRemote {
			return domain.ErrValidation
		}
		query := "SELECT unixepoch()"
		if tx.Dialect().Name() == dialect.MySQL {
			query = "SELECT UNIX_TIMESTAMP()"
		}
		var now int64
		if err := tx.NewRaw(query).Scan(ctx, &now); err != nil {
			return err
		}
		// Reset preparation revokes effective admission without changing the
		// operator's enabled preference. Release callbacks can still clean up.
		enabled := probe.Enabled
		if err := requireNoStreamReset(ctx, tx, probeID, false); err != nil {
			if !errors.Is(err, ports.ErrConflict) {
				return err
			}
			enabled = false
		}
		return action(ctx, tx, enabled, now)
	})
	if err != nil {
		return fmt.Errorf("probe connector transaction: %w", probeRegistryError(err))
	}
	return nil
}

func readProbeSession(ctx context.Context, tx bun.Tx, probeID string) (probeSessionModel, error) {
	var row probeSessionModel
	q := tx.NewSelect().Model(&row).Where("probe_id = ?", probeID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	err := q.Scan(ctx)
	return row, err
}

// AcquireConnector is the legacy acquisition path for a probe not yet adopted
// by a runtime owner. Adopted probes require AcquireRuntimeConnector.
func (s *ProbeConnectorStore) AcquireConnector(ctx context.Context, probeID, ownerID string) (domain.ProbeConnectorLease, error) {
	if !domain.ValidHubID(ownerID) {
		return domain.ProbeConnectorLease{}, domain.ErrValidation
	}
	var result domain.ProbeConnectorLease
	err := s.transaction(ctx, probeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		if !enabled {
			return ports.ErrConflict
		}
		// Once adopted, every subsequent attempt must present the runtime epoch.
		if _, err := readProbeRuntime(ctx, tx, probeID); !errors.Is(err, sql.ErrNoRows) {
			if err != nil {
				return err
			}
			return ports.ErrConflict
		}
		var err error
		result, err = acquireProbeConnector(ctx, tx, probeID, ownerID, now, now+60)
		return err
	})
	if err != nil {
		return domain.ProbeConnectorLease{}, err
	}
	return result, nil
}

func acquireProbeConnector(ctx context.Context, tx bun.Tx, probeID, ownerID string, now, until int64) (domain.ProbeConnectorLease, error) {
	row, err := readProbeSession(ctx, tx, probeID)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return domain.ProbeConnectorLease{}, err
	}
	if !missing && (row.LeaseUntil > now && row.OwnerID != ownerID || row.Generation == math.MaxInt64) {
		return domain.ProbeConnectorLease{}, ports.ErrConflict
	}
	row = probeSessionModel{ProbeID: probeID, OwnerID: ownerID, Generation: row.Generation + 1, LeaseUntil: min(now+60, until)}
	if missing {
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
	} else {
		_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
	}
	return row.lease(), err
}

func matchesProbeSession(row probeSessionModel, lease domain.ProbeConnectorLease) bool {
	return row.OwnerID == lease.OwnerID && row.Generation == lease.Generation && lease.Generation > 0
}

// RenewConnector retains the fence only while the prior lease is still alive.
func (s *ProbeConnectorStore) RenewConnector(ctx context.Context, lease domain.ProbeConnectorLease) (domain.ProbeConnectorLease, error) {
	var result domain.ProbeConnectorLease
	err := s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		row, err := readProbeSession(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !enabled || !matchesProbeSession(row, lease) || row.LeaseUntil <= now {
			return ports.ErrConflict
		}
		row.LeaseUntil, err = connectorLeaseDeadline(ctx, tx, row, now)
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		result = row.lease()
		return nil
	})
	if err != nil {
		return domain.ProbeConnectorLease{}, err
	}
	return result, nil
}

// ReleaseConnector retains its generation so a stale owner can never reuse it.
func (s *ProbeConnectorStore) ReleaseConnector(ctx context.Context, lease domain.ProbeConnectorLease) error {
	return s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, _ bool, _ int64) error {
		row, err := readProbeSession(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !matchesProbeSession(row, lease) {
			return ports.ErrConflict
		}
		row.OwnerID, row.LeaseUntil, row.Connected = "", 0, false
		_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		return err
	})
}

// SetConnectorConnected ignores neither expired nor stale close callbacks.
func (s *ProbeConnectorStore) SetConnectorConnected(ctx context.Context, lease domain.ProbeConnectorLease, connected bool) error {
	return s.transaction(ctx, lease.ProbeID, func(ctx context.Context, tx bun.Tx, enabled bool, now int64) error {
		row, err := readProbeSession(ctx, tx, lease.ProbeID)
		if err != nil {
			return err
		}
		if !matchesProbeSession(row, lease) || row.LeaseUntil <= now || connected && !enabled {
			return ports.ErrConflict
		}
		row.Connected = connected
		_, err = tx.NewUpdate().Model(&row).WherePK().Exec(ctx)
		return err
	})
}
