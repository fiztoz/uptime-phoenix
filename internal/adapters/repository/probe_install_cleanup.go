package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeInstallCleanupModel stores one durable cleanup: the nonsecret request
// (the install identity itself stays in probe_install_operations and is read
// through the shared authority row), the immutable owner, the fixed lifecycle
// fields and the single reserved attempt. The cleanup's deployment private key,
// key-file paths and raw remote output have no column by construction.
type probeInstallCleanupModel struct {
	bun.BaseModel        `bun:"table:probe_install_cleanups"`
	CleanupID            string    `bun:"cleanup_id,pk"`
	OperationID          string    `bun:"operation_id"`
	ClientKeyFingerprint string    `bun:"client_key_fingerprint"`
	RequestedBy          int64     `bun:"requested_by"`
	OwnerID              string    `bun:"owner_id"`
	State                string    `bun:"state"`
	ErrorCode            *string   `bun:"error_code"`
	AttemptID            *string   `bun:"attempt_id"`
	CreatedAt            time.Time `bun:"created_at"`
	UpdatedAt            time.Time `bun:"updated_at"`
	DeadlineAt           time.Time `bun:"deadline_at"`
}

// probeCleanupAdmissionModel is the one consumed cleanup attempt. It mirrors
// hostActionAdmissionModel against the cleanup row that reserved the attempt.
type probeCleanupAdmissionModel struct {
	bun.BaseModel  `bun:"table:probe_cleanup_admissions"`
	AttemptID      string    `bun:"attempt_id,pk"`
	CleanupID      string    `bun:"cleanup_id"`
	OwnerID        string    `bun:"owner_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

// probeInstallCleanupGraph is the locked shared install operation (graph) plus
// one cleanup row. It is only built from a current read taken under the
// operation row lock and is validated as a whole before it is used.
type probeInstallCleanupGraph struct {
	install probeInstallGraph
	cleanup probeInstallCleanupModel
}

// probeCleanupTerminal reports whether the stored cleanup may no longer change
// outcome. Cleaned is the one success and is as immutable as a failure.
func probeCleanupTerminal(state string) bool {
	switch state {
	case domain.ProbeCleanupCleaned, domain.ProbeCleanupFailed, domain.ProbeCleanupInterrupted:
		return true
	}
	return false
}

// probeCleanupSettled maps the one executor report onto the cleanup's terminal
// state and error code: applied is the only success, not_started is definite
// and unknown means resources may be partly removed.
func probeCleanupSettled(outcome domain.ProbeInstallResourceOutcome) (string, string) {
	switch outcome.State {
	case domain.ProbeInstallResourceApplied:
		return domain.ProbeCleanupCleaned, ""
	case domain.ProbeInstallResourceNotStarted:
		return domain.ProbeCleanupFailed, "not_started"
	}
	return domain.ProbeCleanupInterrupted, "outcome_unknown"
}

// receipt assembles the complete stored cleanup and fails closed with
// domain.ErrValidation on corruption: the install graph, the cleanup row and
// every timestamp must satisfy the frozen domain contract, or the row is
// refused instead of being served or repaired.
func (g probeInstallCleanupGraph) receipt() (domain.ProbeCleanup, error) {
	install, err := g.install.receipt()
	if err != nil {
		return domain.ProbeCleanup{}, err
	}
	m := g.cleanup
	if m.OperationID != install.Identity.OperationID {
		return domain.ProbeCleanup{}, fmt.Errorf("cleanup %s belongs to %s, not install %s: %w", m.CleanupID, m.OperationID, install.Identity.OperationID, domain.ErrValidation)
	}
	out := domain.ProbeCleanup{
		Identity: domain.ProbeCleanupIdentity{
			CleanupID: m.CleanupID, Install: install.Identity,
			ClientKeyFingerprint: m.ClientKeyFingerprint, RequestedBy: m.RequestedBy,
		},
		OwnerID:    m.OwnerID,
		State:      m.State,
		CreatedAt:  m.CreatedAt.UTC(),
		UpdatedAt:  m.UpdatedAt.UTC(),
		DeadlineAt: m.DeadlineAt.UTC(),
	}
	if m.ErrorCode != nil {
		if *m.ErrorCode == "" {
			return domain.ProbeCleanup{}, fmt.Errorf("empty cleanup error code on %s: %w", m.CleanupID, domain.ErrValidation)
		}
		out.ErrorCode = *m.ErrorCode
	}
	if m.AttemptID != nil {
		if *m.AttemptID == "" {
			return domain.ProbeCleanup{}, fmt.Errorf("empty cleanup attempt on %s: %w", m.CleanupID, domain.ErrValidation)
		}
		out.AttemptID = *m.AttemptID
	}
	if !out.Valid() {
		return domain.ProbeCleanup{}, fmt.Errorf("corrupt probe install cleanup %s: %w", m.CleanupID, domain.ErrValidation)
	}
	return out, nil
}

// readProbeCleanupRow reads one cleanup row with a current locking read. found
// is false when no such cleanup exists.
func readProbeCleanupRow(ctx context.Context, tx bun.Tx, cleanupID string) (probeInstallCleanupModel, bool, error) {
	row := new(probeInstallCleanupModel)
	q := tx.NewSelect().Model(row).Where("cleanup_id = ?", cleanupID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeInstallCleanupModel{}, false, nil
		}
		return probeInstallCleanupModel{}, false, err
	}
	return *row, true, nil
}

// readProbeCleanupByAttempt reads the one cleanup of an install operation that
// reserved attemptID. The operation is part of the predicate, so a cleanup of
// any other operation is never read (or locked) from this transaction.
func readProbeCleanupByAttempt(ctx context.Context, tx bun.Tx, operationID, attemptID string) (probeInstallCleanupModel, bool, error) {
	row := new(probeInstallCleanupModel)
	q := tx.NewSelect().Model(row).Where("operation_id = ? AND attempt_id = ?", operationID, attemptID)
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeInstallCleanupModel{}, false, nil
		}
		return probeInstallCleanupModel{}, false, err
	}
	return *row, true, nil
}

// readProbeCleanupRows reads the cleanups of one install operation newest
// first with current locking reads. Ordering is created_at then cleanup_id,
// both descending: a timestamp is never assumed unique, so the cleanup ID is the
// deterministic tie-break. limit 0 reads every row.
func readProbeCleanupRows(ctx context.Context, tx bun.Tx, operationID string, limit int) ([]probeInstallCleanupModel, error) {
	var rows []probeInstallCleanupModel
	q := tx.NewSelect().Model(&rows).Where("operation_id = ?", operationID).Order("created_at DESC", "cleanup_id DESC")
	if limit > 0 {
		q = q.Limit(int64(limit))
	}
	if tx.Dialect().Name() == dialect.MySQL {
		q = q.For("UPDATE")
	}
	if err := q.Scan(ctx); err != nil {
		return nil, err
	}
	return rows, nil
}

// setLifecycle updates the lifecycle columns of the locked cleanup row and
// mirrors them into the in-memory graph so the receipt is rebuilt from exactly
// what was written. The attempt is only ever set by BeginCleanup.
func (g *probeInstallCleanupGraph) setLifecycle(ctx context.Context, tx bun.Tx, state string, code *string, updated time.Time) error {
	if _, err := tx.NewUpdate().Table("probe_install_cleanups").
		Set("state = ?", state).
		Set("error_code = ?", code).
		Set("updated_at = ?", updated).
		Where("cleanup_id = ?", g.cleanup.CleanupID).Exec(ctx); err != nil {
		return err
	}
	g.cleanup.State = state
	g.cleanup.ErrorCode = code
	g.cleanup.UpdatedAt = updated
	return nil
}

// reconcileExpiry records honest cold-expiry state: a hard-expired queued or
// running cleanup becomes interrupted/deadline_expired and keeps whatever
// attempt it reserved. Terminal cleanups are immutable. The caller must commit
// this even when it then refuses the requested transition.
func (g *probeInstallCleanupGraph) reconcileExpiry(ctx context.Context, tx bun.Tx, now time.Time) (bool, error) {
	c := &g.cleanup
	if probeCleanupTerminal(c.State) || installJournalTime(now).Before(installJournalTime(c.DeadlineAt)) {
		return false, nil
	}
	code := "deadline_expired"
	if err := g.setLifecycle(ctx, tx, domain.ProbeCleanupInterrupted, &code, installJournalStamp(c.UpdatedAt, now)); err != nil {
		return false, err
	}
	return true, nil
}

// loadProbeCleanupGraph locks the shared install operation row first
// (operation-first locking, exactly like the install journal, the host-action
// admissions and the activation), reads the cleanup row with a current read and
// validates the whole graph before any expiry reconciliation or transition is
// considered. A missing install or cleanup is ports.ErrNotFound; corruption is
// domain.ErrValidation.
func loadProbeCleanupGraph(ctx context.Context, tx bun.Tx, operationID, cleanupID string) (probeInstallCleanupGraph, error) {
	install, err := loadProbeInstallGraph(ctx, tx, operationID)
	if err != nil {
		return probeInstallCleanupGraph{}, err
	}
	row, found, err := readProbeCleanupRow(ctx, tx, cleanupID)
	if err != nil {
		return probeInstallCleanupGraph{}, err
	}
	if !found {
		return probeInstallCleanupGraph{}, ports.ErrNotFound
	}
	graph := probeInstallCleanupGraph{install: install, cleanup: row}
	if _, err := graph.receipt(); err != nil {
		return probeInstallCleanupGraph{}, err
	}
	return graph, nil
}

// ProbeInstallCleanupStore persists the M8 durable install cleanup and its
// one-shot host-action admission on either hub engine. It mirrors
// ProbeInstallActivationStore: every mutation is transactional, serializes on
// the shared install operation row first, validates the complete stored graph
// before any expiry reconciliation, samples the DB UTC clock after that lock and
// fences the current stored owner, state and deadline in the same transaction.
// There is no renewal, takeover, resume, adoption, rearm, delete or retention
// API: a failed or interrupted cleanup is retried only as a new cleanup.
type ProbeInstallCleanupStore struct{ db *bun.DB }

// NewProbeInstallCleanupStore creates a Bun-backed cleanup store over an
// existing connection pool. It starts no work and owns no connection lifetime.
func NewProbeInstallCleanupStore(db *bun.DB) *ProbeInstallCleanupStore {
	return &ProbeInstallCleanupStore{db: db}
}

var _ ports.ProbeCleanupRepository = (*ProbeInstallCleanupStore)(nil)
var _ ports.ProbeCleanupAdmissionRepository = (*ProbeInstallCleanupStore)(nil)

// operationOf resolves the install operation one cleanup belongs to. It runs
// outside any transaction on purpose: the binding is immutable (no update or
// delete path exists and the FK restricts the install's deletion), so a
// pre-read can never go stale, and it must not hold a lock on the cleanup row
// before the transaction takes the operation lock first.
func (s *ProbeInstallCleanupStore) operationOf(ctx context.Context, cleanupID string) (string, error) {
	var operationID string
	if err := s.db.NewSelect().Table("probe_install_cleanups").Column("operation_id").
		Where("cleanup_id = ?", cleanupID).Scan(ctx, &operationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ports.ErrNotFound
		}
		return "", err
	}
	return operationID, nil
}

// CreateCleanup commits one queued cleanup atomically before acknowledgement.
// Under the install operation row lock, after the DB clock is sampled and the
// install's own hard expiry is reconciled, the install must equal
// identity.Install exactly and be cleanup-eligible; its activation, if any, must
// be terminal (an expired one is reconciled first); and no other cleanup of the
// operation may be nonterminal (an expired one is reconciled first) or cleaned.
// A new cleanup never sorts before an earlier one, even if the DB clock moved
// backwards, because its creation stamp is clamped to the newest stored stamp.
// An exact identity retry returns the original receipt with created=false and
// never replenishes authority, whatever owner the retrying process presents; a
// changed identity conflicts.
func (s *ProbeInstallCleanupStore) CreateCleanup(ctx context.Context, identity domain.ProbeCleanupIdentity, ownerID string) (*domain.ProbeCleanup, bool, error) {
	if s == nil || s.db == nil || !identity.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, false, fmt.Errorf("invalid probe cleanup create request: %w", domain.ErrValidation)
	}
	operationID := identity.Install.OperationID
	var out *domain.ProbeCleanup
	var commit installJournalCommit
	for attempt := 0; ; attempt++ {
		created := false
		err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			out, created = nil, false
			commit.begin()
			install, err := loadProbeInstallGraph(ctx, tx, operationID)
			if err != nil {
				return err
			}
			existing, found, err := readProbeCleanupRow(ctx, tx, identity.CleanupID)
			if err != nil {
				return err
			}
			if found {
				if existing.OperationID != operationID {
					return ports.ErrConflict
				}
				graph := probeInstallCleanupGraph{install: install, cleanup: existing}
				stored, err := graph.receipt()
				if err != nil {
					return err
				}
				if stored.Identity != identity {
					return ports.ErrConflict
				}
				// Exact identity retry: the stored receipt stands untouched,
				// including its owner, deadline and outcome.
				out = &stored
				return nil
			}
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			now = installJournalTime(now)
			reconciled, err := install.reconcileExpiry(ctx, tx, now)
			if err != nil {
				return err
			}
			commit.reconciled = reconciled
			operation, err := install.receipt()
			if err != nil {
				return err
			}
			if operation.Identity != identity.Install {
				return commit.fail(ports.ErrConflict)
			}
			if !domain.ProbeInstallCleanupEligible(operation.State) {
				return commit.fail(domain.ErrProbeCleanupIneligible)
			}
			// An activation may still be creating, registering or starting the
			// probe: only a terminal one leaves no work. A hard-expired one is
			// reconciled (and committed) first, like every other reader does.
			activation, steps, activationFound, err := readProbeActivationRows(ctx, tx, operationID)
			if err != nil {
				return err
			}
			if activationFound {
				graph := probeInstallActivationGraph{install: install, activation: activation, steps: steps}
				if _, err := graph.receipt(); err != nil {
					return err
				}
				activationReconciled, err := graph.reconcileExpiry(ctx, tx, now)
				if err != nil {
					return err
				}
				commit.reconciled = commit.reconciled || activationReconciled
				current, err := graph.receipt()
				if err != nil {
					return err
				}
				if !current.Terminal() {
					return commit.fail(domain.ErrProbeCleanupIneligible)
				}
			}
			rows, err := readProbeCleanupRows(ctx, tx, operationID, 0)
			if err != nil {
				return err
			}
			for _, row := range rows {
				if _, err := (probeInstallCleanupGraph{install: install, cleanup: row}).receipt(); err != nil {
					return err
				}
			}
			stamp := now
			blocked := false
			for _, row := range rows {
				graph := probeInstallCleanupGraph{install: install, cleanup: row}
				cleanupReconciled, err := graph.reconcileExpiry(ctx, tx, now)
				if err != nil {
					return err
				}
				commit.reconciled = commit.reconciled || cleanupReconciled
				current, err := graph.receipt()
				if err != nil {
					return err
				}
				if !current.Terminal() || current.State == domain.ProbeCleanupCleaned {
					blocked = true
				}
				if floor := installJournalTime(current.UpdatedAt); floor.After(stamp) {
					stamp = floor
				}
			}
			if blocked {
				return commit.fail(domain.ErrProbeCleanupIneligible)
			}
			row := probeInstallCleanupModel{
				CleanupID: identity.CleanupID, OperationID: operationID,
				ClientKeyFingerprint: identity.ClientKeyFingerprint, RequestedBy: identity.RequestedBy,
				OwnerID: ownerID, State: domain.ProbeCleanupQueued,
				CreatedAt: stamp, UpdatedAt: stamp, DeadlineAt: stamp.Add(domain.ProbeCleanupLifetime),
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				if installJournalRaced(err) {
					return errInstallJournalRaced
				}
				return err
			}
			// Read back under the same lock and compare: the receipt is built from
			// what the database actually holds, never from the request.
			locked, lockedFound, err := readProbeCleanupRow(ctx, tx, identity.CleanupID)
			if err != nil {
				return err
			}
			if !lockedFound {
				return fmt.Errorf("cleanup %s vanished inside its own create: %w", identity.CleanupID, domain.ErrValidation)
			}
			stored, err := probeInstallCleanupGraph{install: install, cleanup: locked}.receipt()
			if err != nil {
				return err
			}
			if stored.Identity != identity || stored.OwnerID != ownerID || stored.State != domain.ProbeCleanupQueued {
				return fmt.Errorf("cleanup %s read-back differs from its create: %w", identity.CleanupID, domain.ErrValidation)
			}
			out, created = &stored, true
			return nil
		})
		if errors.Is(err, errInstallJournalRaced) && attempt+1 < installJournalAttemptLimit {
			continue
		}
		if errors.Is(err, errInstallJournalRaced) {
			// Exhausted contention proves no stored identity mismatch. Keep retries
			// bounded, but report unavailability rather than inventing a permanent
			// conflict.
			return nil, false, domain.ErrInternal
		}
		if err != nil {
			return nil, false, err
		}
		if commit.pending != nil {
			return nil, false, commit.pending
		}
		return out, created, nil
	}
}

// ObserveCleanup returns the durable cleanup. A hard-expired queued or running
// cleanup is reconciled to interrupted/deadline_expired first, atomically and
// after the complete graph has been validated. Reads never create, dispatch,
// renew or grant anything.
func (s *ProbeInstallCleanupStore) ObserveCleanup(ctx context.Context, cleanupID string) (*domain.ProbeCleanup, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(cleanupID) {
		return nil, fmt.Errorf("invalid probe cleanup id: %w", domain.ErrValidation)
	}
	operationID, err := s.operationOf(ctx, cleanupID)
	if err != nil {
		return nil, err
	}
	var out *domain.ProbeCleanup
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		graph, err := loadProbeCleanupGraph(ctx, tx, operationID, cleanupID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := graph.reconcileExpiry(ctx, tx, now); err != nil {
			return err
		}
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LatestCleanup returns the newest cleanup of an install operation (created_at,
// then cleanup_id, descending), reconciled exactly like ObserveCleanup, or
// ports.ErrNotFound when the operation has none (or does not exist). At most
// one cleanup of an operation is ever nonterminal and it is always the newest,
// so reconciling the newest row is complete.
func (s *ProbeInstallCleanupStore) LatestCleanup(ctx context.Context, operationID string) (*domain.ProbeCleanup, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid probe cleanup operation id: %w", domain.ErrValidation)
	}
	var out *domain.ProbeCleanup
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		install, err := loadProbeInstallGraph(ctx, tx, operationID)
		if err != nil {
			return err
		}
		rows, err := readProbeCleanupRows(ctx, tx, operationID, 1)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return ports.ErrNotFound
		}
		graph := probeInstallCleanupGraph{install: install, cleanup: rows[0]}
		if _, err := graph.receipt(); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := graph.reconcileExpiry(ctx, tx, now); err != nil {
			return err
		}
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &current
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// BeginCleanup is the single queued-to-running transition and the dispatch
// grant for exactly one executor. It requires the exact stored owner, queued
// state and an unexpired deadline, and reserves attemptID, which must be unique
// across every cleanup; a second begin can never dispatch again and nothing here
// ever renews, retakes or rebinds the owner.
func (s *ProbeInstallCleanupStore) BeginCleanup(ctx context.Context, cleanupID, ownerID, attemptID string) (*domain.ProbeCleanup, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(cleanupID) ||
		!domain.ValidProbeOperationID(ownerID) || !domain.ValidProbeOperationID(attemptID) {
		return nil, fmt.Errorf("invalid probe cleanup begin request: %w", domain.ErrValidation)
	}
	operationID, err := s.operationOf(ctx, cleanupID)
	if err != nil {
		return nil, err
	}
	var out *domain.ProbeCleanup
	var commit installJournalCommit
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeCleanupGraph(ctx, tx, operationID, cleanupID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.OwnerID != ownerID || current.State != domain.ProbeCleanupQueued || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		var taken int
		if err := tx.NewSelect().Table("probe_install_cleanups").
			ColumnExpr("COUNT(*)").Where("attempt_id = ?", attemptID).Scan(ctx, &taken); err != nil {
			return err
		}
		if taken > 0 {
			// Attempt UUIDs are unique across the whole cleanup journal.
			return commit.fail(ports.ErrConflict)
		}
		updated := installJournalStamp(graph.cleanup.UpdatedAt, now)
		if _, err := tx.NewUpdate().Table("probe_install_cleanups").
			Set("state = ?", domain.ProbeCleanupRunning).
			Set("attempt_id = ?", attemptID).
			Set("updated_at = ?", updated).
			Where("cleanup_id = ?", cleanupID).Exec(ctx); err != nil {
			if installJournalRaced(err) {
				return commit.fail(ports.ErrConflict)
			}
			return err
		}
		graph.cleanup.State = domain.ProbeCleanupRunning
		graph.cleanup.AttemptID = &attemptID
		graph.cleanup.UpdatedAt = updated
		begun, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &begun
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}

// RecordCleanup settles a running cleanup for its exact stored owner and
// attempt: applied becomes cleaned, not_started failed/not_started and unknown
// interrupted/outcome_unknown. A cleanup whose hard deadline has passed is
// reconciled to interrupted/deadline_expired instead and that receipt is
// returned: a late executor report never replaces expiry uncertainty. Settled
// receipts are immutable history: an exact outcome retry reads that history
// without mutation and a different outcome conflicts.
func (s *ProbeInstallCleanupStore) RecordCleanup(ctx context.Context, cleanupID, ownerID, attemptID string, outcome domain.ProbeInstallResourceOutcome) (*domain.ProbeCleanup, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(cleanupID) || !domain.ValidProbeOperationID(ownerID) ||
		!domain.ValidProbeOperationID(attemptID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid probe cleanup record request: %w", domain.ErrValidation)
	}
	operationID, err := s.operationOf(ctx, cleanupID)
	if err != nil {
		return nil, err
	}
	state, errorCode := probeCleanupSettled(outcome)
	var out *domain.ProbeCleanup
	var commit installJournalCommit
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeCleanupGraph(ctx, tx, operationID, cleanupID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.OwnerID != ownerID || current.AttemptID == "" || current.AttemptID != attemptID {
			// Record validates the exact stored owner and attempt.
			return commit.fail(ports.ErrConflict)
		}
		if current.Terminal() {
			expired := current.State == domain.ProbeCleanupInterrupted && current.ErrorCode == "deadline_expired"
			if expired || (current.State == state && current.ErrorCode == errorCode) {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.State != domain.ProbeCleanupRunning {
			return commit.fail(ports.ErrConflict)
		}
		// The deadline check is implied: a running cleanup at or past its deadline
		// was just reconciled to the terminal expired receipt above.
		var code *string
		if errorCode != "" {
			code = &errorCode
		}
		if err := graph.setLifecycle(ctx, tx, state, code, installJournalStamp(graph.cleanup.UpdatedAt, now)); err != nil {
			return err
		}
		recorded, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &recorded
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}

// AbandonCleanup ends a still-queued cleanup of ownerID as
// interrupted/owner_interrupted. It never touches a running cleanup (that is a
// conflict: a joined executor owns its outcome). A queued cleanup whose
// deadline already passed is reconciled to interrupted/deadline_expired and
// that receipt is returned, since the cleanup is interrupted either way; exact
// retries of an earlier abandon read the same history.
func (s *ProbeInstallCleanupStore) AbandonCleanup(ctx context.Context, cleanupID, ownerID string) (*domain.ProbeCleanup, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(cleanupID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, fmt.Errorf("invalid probe cleanup abandon request: %w", domain.ErrValidation)
	}
	operationID, err := s.operationOf(ctx, cleanupID)
	if err != nil {
		return nil, err
	}
	var out *domain.ProbeCleanup
	var commit installJournalCommit
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		commit.begin()
		graph, err := loadProbeCleanupGraph(ctx, tx, operationID, cleanupID)
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.OwnerID != ownerID {
			return commit.fail(ports.ErrConflict)
		}
		if current.Terminal() {
			// Only an interruption of a cleanup that never began is the abandon's
			// own outcome; everything else is somebody else's immutable history.
			if current.State == domain.ProbeCleanupInterrupted && current.AttemptID == "" {
				out = &current
				return nil
			}
			return commit.fail(ports.ErrConflict)
		}
		if current.State != domain.ProbeCleanupQueued {
			return commit.fail(ports.ErrConflict)
		}
		code := "owner_interrupted"
		if err := graph.setLifecycle(ctx, tx, domain.ProbeCleanupInterrupted, &code, installJournalStamp(graph.cleanup.UpdatedAt, now)); err != nil {
			return err
		}
		abandoned, err := graph.receipt()
		if err != nil {
			return err
		}
		out = &abandoned
		return nil
	})
	if err != nil {
		return nil, err
	}
	if commit.pending != nil {
		return nil, commit.pending
	}
	return out, nil
}

// AdmitCleanupAction locks current authority and durably consumes exactly one
// cleanup attempt, only while the cleanup is running under the intent's owner
// with the intent's attempt, its install identity equals the intent's, and the
// fixed deadline has not passed. The insert is plain: every uniqueness
// collision refuses, even an exact replay, and the successful insert itself
// proves fresh consumption. All failures return a zero admission, including
// ambiguous commit acknowledgment and caller cancellation after commit. A later
// expiry does not retroactively revoke an admitted remote action.
func (s *ProbeInstallCleanupStore) AdmitCleanupAction(ctx context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionAdmission, error) {
	var zero domain.ProbeHostActionAdmission
	if ctx == nil || (reflect.ValueOf(ctx).Kind() == reflect.Pointer && reflect.ValueOf(ctx).IsNil()) {
		return zero, domain.ErrValidation
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if !request.Valid() {
		return zero, domain.ErrValidation
	}
	// Install and activation host actions belong to their own admission stores
	// and are never consumable here.
	if request.Intent.Kind != domain.ProbeHostActionCleanup {
		return zero, domain.ErrValidation
	}
	if s == nil || s.db == nil {
		return zero, domain.ErrInternal
	}
	var out domain.ProbeHostActionAdmission
	var commit installJournalCommit
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = zero
		commit.begin()
		intent := request.Intent
		install, err := loadProbeInstallGraph(ctx, tx, intent.Identity.OperationID)
		if err != nil {
			return err
		}
		row, found, err := readProbeCleanupByAttempt(ctx, tx, intent.Identity.OperationID, intent.AttemptID)
		if err != nil {
			return err
		}
		if !found {
			// No cleanup of this operation reserved this attempt.
			return ports.ErrConflict
		}
		graph := probeInstallCleanupGraph{install: install, cleanup: row}
		if _, err := graph.receipt(); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		reconciled, err := graph.reconcileExpiry(ctx, tx, now)
		if err != nil {
			return err
		}
		commit.reconciled = reconciled
		current, err := graph.receipt()
		if err != nil {
			return err
		}
		if current.Identity.Install != intent.Identity || current.OwnerID != intent.OwnerID ||
			current.AttemptID != intent.AttemptID || current.State != domain.ProbeCleanupRunning || !now.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		stamp := installJournalStamp(current.UpdatedAt, now)
		if !stamp.Before(current.DeadlineAt) {
			return commit.fail(ports.ErrConflict)
		}
		admission := probeCleanupAdmissionModel{
			AttemptID: intent.AttemptID, CleanupID: current.Identity.CleanupID, OwnerID: intent.OwnerID,
			AuthorityKeyID: request.Challenge.AuthorityKeyID, NonceHex: hex.EncodeToString(request.Challenge.Nonce[:]),
			MarkerSHA256: request.Challenge.MarkerSHA256, AdmittedAt: stamp,
		}
		// Never IGNORE/upsert: the successful insert itself proves fresh
		// consumption. Every uniqueness collision refuses, even exact replay.
		if _, err := tx.NewInsert().Model(&admission).Exec(ctx); err != nil {
			if installJournalRaced(err) {
				return ports.ErrConflict
			}
			return err
		}
		if _, err := tx.NewUpdate().Table("probe_install_cleanups").Set("updated_at = ?", stamp).
			Where("cleanup_id = ?", admission.CleanupID).Exec(ctx); err != nil {
			return err
		}
		var saved probeCleanupAdmissionModel
		if err := tx.NewSelect().Model(&saved).Where("attempt_id = ?", admission.AttemptID).Scan(ctx); err != nil {
			return err
		}
		if saved.AttemptID != admission.AttemptID || saved.CleanupID != admission.CleanupID || saved.OwnerID != admission.OwnerID ||
			saved.AuthorityKeyID != admission.AuthorityKeyID || saved.NonceHex != admission.NonceHex ||
			saved.MarkerSHA256 != admission.MarkerSHA256 || !saved.AdmittedAt.Equal(stamp) {
			return domain.ErrValidation
		}
		admitted := domain.ProbeHostActionAdmission{Request: request, AdmittedAt: saved.AdmittedAt.UTC()}
		if !admitted.Valid() {
			return domain.ErrValidation
		}
		out = admitted
		return nil
	})
	if err != nil {
		return zero, binaryStageAdmissionError(err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if commit.pending != nil {
		return zero, binaryStageAdmissionError(commit.pending)
	}
	if !out.Valid() || out.Request != request {
		return zero, domain.ErrInternal
	}
	return out, nil
}
