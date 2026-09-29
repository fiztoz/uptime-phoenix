package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// maxUnawareWorkersReported bounds how many worker identities a single readiness
// query returns. An activation refusal names the offenders, and that message
// reaches an API response and a log line, so it must stay bounded no matter how
// large a fleet gets or how badly a rollout goes.
const maxUnawareWorkersReported = 32

// hubWorkerCapabilityModel is one row of the assignment-ownership attestation
// table. It carries no credential material: a worker identity and the protocol
// version it enforces are fleet metadata, not secrets.
type hubWorkerCapabilityModel struct {
	bun.BaseModel      `bun:"table:hub_worker_capabilities"`
	WorkerID           string    `bun:"worker_id,pk"`
	AssignmentProtocol int       `bun:"assignment_protocol"`
	DeclaredAt         time.Time `bun:"declared_at"`
	LeaseUntil         time.Time `bun:"lease_until"`
}

// unawareWorkerRow reads one offender identity from the readiness query. It is a
// model rather than a bare []string so the scan uses Bun's ordinary column
// mapping on both engines.
type unawareWorkerRow struct {
	WorkerID string `bun:"worker_id"`
}

// HubWorkerReadinessStore is the shared, dialect-neutral implementation of
// ports.HubWorkerReadiness. One Bun store serves both hub engines because the
// attestation queries use only predicates MariaDB and SQLite both support; there
// is no engine-specific SQL to diverge.
type HubWorkerReadinessStore struct {
	db *bun.DB
}

// NewHubWorkerReadinessStore binds the attestation store to a hub database.
func NewHubWorkerReadinessStore(db *bun.DB) *HubWorkerReadinessStore {
	return &HubWorkerReadinessStore{db: db}
}

var _ ports.HubWorkerReadiness = (*HubWorkerReadinessStore)(nil)

// DeclareWorker records or refreshes one worker's attestation inside a single
// transaction, and prunes attestations whose lease has already expired so the
// table cannot grow without bound across restarts and rescaled fleets.
//
// The refresh is update-then-insert rather than an engine-specific upsert: the
// two engines spell conflict handling differently, and a worker identity is
// owned by exactly one process, so the race this ordering could expose is not
// reachable. Every timestamp written here is UTC, because the liveness
// comparison in UnawareWorkers is a wall-clock comparison against the same
// column and a local-zoned bound would silently shift the window.
func (s *HubWorkerReadinessStore) DeclareWorker(ctx context.Context, workerID string, protocol int, ttl time.Duration) error {
	if workerID == "" || protocol < 0 || ttl <= 0 {
		return fmt.Errorf("declare hub worker capability: %w", domain.ErrValidation)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(ttl)
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewDelete().Table("hub_worker_capabilities").
			Where("lease_until < ?", now).Exec(ctx); err != nil {
			return fmt.Errorf("prune expired hub worker capabilities: %w", err)
		}
		res, err := tx.NewUpdate().Table("hub_worker_capabilities").
			Set("assignment_protocol = ?", protocol).
			Set("declared_at = ?", now).
			Set("lease_until = ?", leaseUntil).
			Where("worker_id = ?", workerID).Exec(ctx)
		if err != nil {
			return fmt.Errorf("refresh hub worker capability: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		row := &hubWorkerCapabilityModel{
			WorkerID: workerID, AssignmentProtocol: protocol,
			DeclaredAt: now, LeaseUntil: leaseUntil,
		}
		if _, err := tx.NewInsert().Model(row).Exec(ctx); err != nil {
			return fmt.Errorf("insert hub worker capability: %w", err)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("declare hub worker capability: %w", err)
	}
	return nil
}

// UnawareWorkers returns live worker identities that cannot be shown to enforce
// requiredProtocol. Two populations qualify, and both matter:
//
//   - a worker holding a live monitor lease with no current attestation at the
//     required protocol. This is the mixed-version case: an older binary never
//     writes an attestation but does claim leases, so its lease is the evidence.
//   - a worker whose attestation is current but declares an older protocol. It
//     may hold no lease right now, yet it is alive, self-identified as unaware,
//     and could claim work at any moment.
//
// A dead worker is deliberately absent from both: once its lease ages past
// leaseLookback and its attestation expires it executes nothing, so it must not
// block activation forever. That is why the caller's leaseLookback has to cover
// the fleet's shard lease TTL — a shorter window would let a running unaware
// worker look dead and quietly defeat the gate.
func (s *HubWorkerReadinessStore) UnawareWorkers(ctx context.Context, requiredProtocol int, leaseLookback time.Duration) ([]string, error) {
	if requiredProtocol < 0 || leaseLookback <= 0 {
		return nil, fmt.Errorf("list unaware hub workers: %w", domain.ErrValidation)
	}
	now := time.Now().UTC()
	leaseCutoff := now.Add(-leaseLookback)
	// UNION (not UNION ALL) is what makes the two populations distinct without a
	// second deduplication pass. The derived table needs an alias for MariaDB.
	const query = `
SELECT worker_id FROM (
    SELECT m.worker_id AS worker_id
      FROM monitors m
     WHERE m.worker_id IS NOT NULL
       AND m.leased_at >= ?
       AND NOT EXISTS (
             SELECT 1 FROM hub_worker_capabilities c
              WHERE c.worker_id = m.worker_id
                AND c.assignment_protocol >= ?
                AND c.lease_until >= ?
           )
    UNION
    SELECT c.worker_id AS worker_id
      FROM hub_worker_capabilities c
     WHERE c.lease_until >= ?
       AND c.assignment_protocol < ?
) AS unaware
ORDER BY worker_id
LIMIT ?`
	var rows []unawareWorkerRow
	if err := s.db.NewRaw(query,
		leaseCutoff, requiredProtocol, now, now, requiredProtocol, maxUnawareWorkersReported,
	).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list unaware hub workers: %w", err)
	}
	workers := make([]string, 0, len(rows))
	for _, row := range rows {
		workers = append(workers, row.WorkerID)
	}
	return workers, nil
}
