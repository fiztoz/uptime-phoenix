package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// probeHostPreflightModel stores one durable asynchronous SSH preflight
// discovery receipt. No secret, key path, token, password or raw output column
// exists here by construction; fingerprints are PUBLIC-key fingerprints only.
type probeHostPreflightModel struct {
	bun.BaseModel        `bun:"table:probe_host_preflights"`
	OperationID          string    `bun:"operation_id,pk"`
	SSHAddress           string    `bun:"ssh_address"`
	SSHUser              string    `bun:"ssh_user"`
	HostKeyFingerprint   string    `bun:"host_key_fingerprint"`
	ClientKeyFingerprint string    `bun:"client_key_fingerprint"`
	RequestedBy          int64     `bun:"requested_by"`
	OwnerID              string    `bun:"owner_id"`
	State                string    `bun:"state"`
	ErrorCode            *string   `bun:"error_code"`
	ResultOS             *string   `bun:"result_os"`
	ResultArchitecture   *string   `bun:"result_architecture"`
	ResultDockerCLI      *bool     `bun:"result_docker_cli"`
	ResultSystemd        *bool     `bun:"result_systemd"`
	ResultPrivilege      *string   `bun:"result_privilege"`
	CreatedAt            time.Time `bun:"created_at"`
	UpdatedAt            time.Time `bun:"updated_at"`
	DeadlineAt           time.Time `bun:"deadline_at"`
}

// operation converts one stored row and fails closed on corruption: a row whose
// identity, state, outcome or timestamp ordering violates the frozen domain
// contract is refused with domain.ErrValidation instead of being served.
func (m probeHostPreflightModel) operation() (domain.ProbePreflightOperation, error) {
	out := domain.ProbePreflightOperation{
		Identity: domain.ProbePreflightIdentity{
			OperationID:          m.OperationID,
			Host:                 domain.ProbeSSHHost{Address: m.SSHAddress, User: m.SSHUser},
			HostKeyFingerprint:   m.HostKeyFingerprint,
			ClientKeyFingerprint: m.ClientKeyFingerprint,
			RequestedBy:          m.RequestedBy,
		},
		OwnerID:    m.OwnerID,
		State:      m.State,
		CreatedAt:  m.CreatedAt.UTC(),
		UpdatedAt:  m.UpdatedAt.UTC(),
		DeadlineAt: m.DeadlineAt.UTC(),
	}
	if m.ErrorCode != nil {
		if *m.ErrorCode == "" {
			return domain.ProbePreflightOperation{}, domain.ErrValidation
		}
		out.ErrorCode = *m.ErrorCode
	}
	switch {
	case m.ResultOS != nil || m.ResultArchitecture != nil || m.ResultDockerCLI != nil ||
		m.ResultSystemd != nil || m.ResultPrivilege != nil:
		if m.ResultOS == nil || m.ResultArchitecture == nil || m.ResultDockerCLI == nil ||
			m.ResultSystemd == nil || m.ResultPrivilege == nil {
			return domain.ProbePreflightOperation{}, fmt.Errorf("partial preflight result on %s: %w", m.OperationID, domain.ErrValidation)
		}
		out.Result = &domain.ProbeHostPreflight{
			OS:           *m.ResultOS,
			Architecture: *m.ResultArchitecture,
			DockerCLI:    *m.ResultDockerCLI,
			Systemd:      *m.ResultSystemd,
			Privilege:    *m.ResultPrivilege,
		}
	case out.State == domain.ProbePreflightPassed:
		// Passed without a result violates the outcome contract; fail closed.
		return domain.ProbePreflightOperation{}, fmt.Errorf("passed preflight %s without result: %w", m.OperationID, domain.ErrValidation)
	}
	if !out.Valid() {
		return domain.ProbePreflightOperation{}, fmt.Errorf("corrupt preflight receipt %s: %w", m.OperationID, domain.ErrValidation)
	}
	return out, nil
}

// applyOutcome writes one terminal outcome plus its monotonic updated_at.
func (m *probeHostPreflightModel) applyOutcome(outcome domain.ProbePreflightOutcome, updatedAt time.Time) {
	m.State = outcome.State
	if outcome.ErrorCode != "" {
		code := outcome.ErrorCode
		m.ErrorCode = &code
	} else {
		m.ErrorCode = nil
	}
	if outcome.Result != nil {
		osName, arch, privilege := outcome.Result.OS, outcome.Result.Architecture, outcome.Result.Privilege
		docker, systemd := outcome.Result.DockerCLI, outcome.Result.Systemd
		m.ResultOS, m.ResultArchitecture, m.ResultPrivilege = &osName, &arch, &privilege
		m.ResultDockerCLI, m.ResultSystemd = &docker, &systemd
	} else {
		m.ResultOS, m.ResultArchitecture, m.ResultPrivilege = nil, nil, nil
		m.ResultDockerCLI, m.ResultSystemd = nil, nil
	}
	m.UpdatedAt = updatedAt
}

// preflightStamp keeps updated_at monotonic across DB-clock rollback: a
// rewound clock never moves stored updated_at backwards and grants nothing.
func preflightStamp(stored, now time.Time) time.Time {
	now = now.UTC().Truncate(time.Microsecond)
	previous := stored.UTC()
	if previous.After(now) {
		return previous
	}
	return now
}

// lockProbePreflightRow serializes every stored transition on the receipt row
// and reads it back under that lock. The database clock is sampled by callers
// only after this lock is held.
func lockProbePreflightRow(ctx context.Context, tx bun.Tx, operationID string) (probeHostPreflightModel, error) {
	if _, err := tx.ExecContext(ctx,
		"UPDATE probe_host_preflights SET operation_id = operation_id WHERE operation_id = ?", operationID); err != nil {
		return probeHostPreflightModel{}, err
	}
	var row probeHostPreflightModel
	if err := tx.NewSelect().Model(&row).Where("operation_id = ?", operationID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return probeHostPreflightModel{}, ports.ErrNotFound
		}
		return probeHostPreflightModel{}, err
	}
	return row, nil
}

// preflightIdentityMatches rechecks the complete immutable nonsecret request
// identity of an exact retry. Any changed selector, actor or public key is a
// conflict, never a silent reuse of an earlier operation.
func preflightIdentityMatches(row probeHostPreflightModel, want domain.ProbePreflightIdentity) bool {
	return row.OperationID == want.OperationID &&
		row.SSHAddress == want.Host.Address && row.SSHUser == want.Host.User &&
		row.HostKeyFingerprint == want.HostKeyFingerprint &&
		row.ClientKeyFingerprint == want.ClientKeyFingerprint &&
		row.RequestedBy == want.RequestedBy
}

// preflightOutcomeEqual compares a stored terminal outcome to an exact retry.
func preflightOutcomeEqual(op domain.ProbePreflightOperation, outcome domain.ProbePreflightOutcome) bool {
	if op.State != outcome.State || op.ErrorCode != outcome.ErrorCode {
		return false
	}
	if op.Result == nil || outcome.Result == nil {
		return op.Result == nil && outcome.Result == nil
	}
	return *op.Result == *outcome.Result
}

// preflightDuplicate detects a primary-key race on create so the transaction
// can be re-run and resolve through the exact-retry identity contract.
func preflightDuplicate(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "UNIQUE constraint failed") ||
		strings.Contains(err.Error(), "Duplicate entry"))
}

// preflightLocked detects SQLite's read-to-write upgrade race between two
// separate connections: the loser aborts immediately instead of waiting on the
// busy handler, so the whole transaction must be re-run.
func preflightLocked(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "database is locked") ||
		strings.Contains(err.Error(), "SQLITE_BUSY"))
}

// errPreflightRaced aborts (and thereby rolls back) a create transaction whose
// insert lost a concurrent primary-key race so the caller can re-run it and
// resolve through the exact-retry identity contract.
var errPreflightRaced = errors.New("probe preflight create raced a concurrent writer")

// ProbePreflightStore persists M8 asynchronous SSH host-preflight discovery
// receipts on either hub engine. One implementation serves MariaDB and SQLite
// with identical semantics: every mutation is transactional, samples the DB
// UTC clock after locking the receipt row, and checks current stored owner and
// state; no lease, renewal or takeover exists.
type ProbePreflightStore struct{ db *bun.DB }

// NewProbePreflightStore creates a Bun-backed preflight receipt store.
func NewProbePreflightStore(db *bun.DB) *ProbePreflightStore {
	return &ProbePreflightStore{db: db}
}

var _ ports.ProbePreflightRepository = (*ProbePreflightStore)(nil)

// CreatePreflight commits queued work before acknowledgement. An exact
// immutable identity retry returns the original row (including original owner
// and deadline) with created=false; a changed selector, actor or public key
// conflicts. A primary-key race is resolved by re-running the transaction
// through the same identity contract, never by overwriting history.
func (s *ProbePreflightStore) CreatePreflight(ctx context.Context, request domain.ProbePreflightIdentity, ownerID string) (*domain.ProbePreflightOperation, bool, error) {
	if s == nil || s.db == nil || !request.Valid() || !domain.ValidProbeOperationID(ownerID) {
		return nil, false, fmt.Errorf("invalid preflight create request: %w", domain.ErrValidation)
	}
	var out *domain.ProbePreflightOperation
	created := false
	for attempt := 0; ; attempt++ {
		out, created = nil, false
		err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			now, err := replayDatabaseTime(ctx, tx)
			if err != nil {
				return err
			}
			now = now.UTC().Truncate(time.Microsecond)
			var existing probeHostPreflightModel
			err = tx.NewSelect().Model(&existing).Where("operation_id = ?", request.OperationID).Scan(ctx)
			switch {
			case err == nil:
				if !preflightIdentityMatches(existing, request) {
					return ports.ErrConflict
				}
				op, verr := existing.operation()
				if verr != nil {
					return verr
				}
				out = &op
				return nil
			case errors.Is(err, sql.ErrNoRows):
				// Fresh operation; insert below.
			default:
				return err
			}
			row := probeHostPreflightModel{
				OperationID:          request.OperationID,
				SSHAddress:           request.Host.Address,
				SSHUser:              request.Host.User,
				HostKeyFingerprint:   request.HostKeyFingerprint,
				ClientKeyFingerprint: request.ClientKeyFingerprint,
				RequestedBy:          request.RequestedBy,
				OwnerID:              ownerID,
				State:                domain.ProbePreflightQueued,
				CreatedAt:            now,
				UpdatedAt:            now,
				DeadlineAt:           now.Add(domain.ProbePreflightLifetime),
			}
			if _, ierr := tx.NewInsert().Model(&row).Exec(ctx); ierr != nil {
				if preflightDuplicate(ierr) || preflightLocked(ierr) {
					return errPreflightRaced
				}
				return ierr
			}
			op, verr := row.operation()
			if verr != nil {
				return verr
			}
			out, created = &op, true
			return nil
		})
		if errors.Is(err, errPreflightRaced) && attempt < 3 {
			continue
		}
		if errors.Is(err, errPreflightRaced) {
			// A conflicting concurrent creator kept winning the race; the
			// committed receipt stands and this caller must not assume success.
			return nil, false, ports.ErrConflict
		}
		if err != nil {
			return nil, false, err
		}
		return out, created, nil
	}
}

// ObservePreflight atomically reconciles any hard-expired nonterminal row to
// interrupted before returning it. It never creates work and never infers
// successful completion: interruption is honest cold state, not proof of
// remote quiescence.
func (s *ProbePreflightStore) ObservePreflight(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid preflight operation id: %w", domain.ErrValidation)
	}
	var out *domain.ProbePreflightOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		row, err := lockProbePreflightRow(ctx, tx, operationID)
		if err != nil {
			return err
		}
		// Validate before expiry reconciliation: never repair a corrupt stored
		// lifecycle/outcome by overwriting it with an invented interruption.
		if _, err := row.operation(); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !row.terminalState() && !now.Before(row.DeadlineAt.UTC()) {
			code := "interrupted"
			row.State = domain.ProbePreflightInterrupted
			row.ErrorCode = &code
			row.UpdatedAt = preflightStamp(row.UpdatedAt, now)
			if _, err := tx.NewUpdate().Model(&row).
				Column("state", "error_code", "updated_at").
				WherePK().Exec(ctx); err != nil {
				return err
			}
		}
		op, err := row.operation()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// terminalState reports whether the stored row may no longer change outcome.
func (m probeHostPreflightModel) terminalState() bool {
	switch m.State {
	case domain.ProbePreflightPassed, domain.ProbePreflightFailed,
		domain.ProbePreflightCanceled, domain.ProbePreflightInterrupted:
		return true
	}
	return false
}

// BeginPreflight requires queued state, the exact stored owner and an
// unexpired deadline, then records running. It never renews, retakes or
// changes the owner; a wrong owner or stale state conflicts.
func (s *ProbePreflightStore) BeginPreflight(ctx context.Context, operationID, ownerID string) (*domain.ProbePreflightOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) || !domain.ValidProbeOperationID(ownerID) {
		return nil, fmt.Errorf("invalid preflight begin request: %w", domain.ErrValidation)
	}
	var out *domain.ProbePreflightOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		row, err := lockProbePreflightRow(ctx, tx, operationID)
		if err != nil {
			return err
		}
		current, err := row.operation()
		if err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if current.OwnerID != ownerID || current.State != domain.ProbePreflightQueued || !now.Before(current.DeadlineAt) {
			return ports.ErrConflict
		}
		updated := preflightStamp(row.UpdatedAt, now)
		if _, err := tx.NewUpdate().Model(&row).
			Set("state = ?", domain.ProbePreflightRunning).
			Set("updated_at = ?", updated).
			WherePK().Exec(ctx); err != nil {
			return err
		}
		row.State = domain.ProbePreflightRunning
		row.UpdatedAt = updated
		op, err := row.operation()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// FinishPreflight checks owner, stored state and expiry inside the same
// transaction as the write. Passed/failed require running plus an unexpired
// deadline; canceled requires cancel_requested and the original executor
// having returned; interrupted may be recorded by the joined owner from any
// nonterminal state on shutdown. Exact same-owner outcome retries are
// idempotent but can never replace a historical terminal outcome.
func (s *ProbePreflightStore) FinishPreflight(ctx context.Context, operationID, ownerID string, outcome domain.ProbePreflightOutcome) (*domain.ProbePreflightOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) ||
		!domain.ValidProbeOperationID(ownerID) || !outcome.Valid() {
		return nil, fmt.Errorf("invalid preflight finish request: %w", domain.ErrValidation)
	}
	var out *domain.ProbePreflightOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		row, err := lockProbePreflightRow(ctx, tx, operationID)
		if err != nil {
			return err
		}
		current, err := row.operation()
		if err != nil {
			return err
		}
		if current.Terminal() {
			if current.OwnerID == ownerID && preflightOutcomeEqual(current, outcome) {
				clone := current
				out = &clone
				return nil
			}
			return ports.ErrConflict
		}
		if row.OwnerID != ownerID {
			return ports.ErrConflict
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		unexpired := now.Before(row.DeadlineAt.UTC())
		switch outcome.State {
		case domain.ProbePreflightPassed, domain.ProbePreflightFailed:
			if row.State != domain.ProbePreflightRunning || !unexpired {
				return ports.ErrConflict
			}
		case domain.ProbePreflightCanceled:
			// A joined return permits cancellation only within the original
			// deadline. Expired work is interrupted, never late success/cancel.
			if row.State != domain.ProbePreflightCancelRequested || !unexpired {
				return ports.ErrConflict
			}
		case domain.ProbePreflightInterrupted:
			// Owner shutdown after joining its inspector may record honest
			// interruption from any nonterminal state, including past expiry.
		default:
			return fmt.Errorf("invalid preflight outcome state: %w", domain.ErrValidation)
		}
		updated := preflightStamp(row.UpdatedAt, now)
		row.applyOutcome(outcome, updated)
		if _, err := tx.NewUpdate().Model(&row).
			Column("state", "error_code", "result_os", "result_architecture",
				"result_docker_cli", "result_systemd", "result_privilege", "updated_at").
			WherePK().Exec(ctx); err != nil {
			return err
		}
		op, err := row.operation()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RequestPreflightCancellation persists intent, not a claim that SSH work
// stopped. Terminal rows remain unchanged so a completed preflight is never
// relabeled; the originating executor observes this state by polling and may
// persist canceled only after its inspector returns.
func (s *ProbePreflightStore) RequestPreflightCancellation(ctx context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	if s == nil || s.db == nil || !domain.ValidProbeOperationID(operationID) {
		return nil, fmt.Errorf("invalid preflight cancel request: %w", domain.ErrValidation)
	}
	var out *domain.ProbePreflightOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		row, err := lockProbePreflightRow(ctx, tx, operationID)
		if err != nil {
			return err
		}
		current, err := row.operation()
		if err != nil {
			return err
		}
		if current.Terminal() {
			out = &current
			return nil
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(current.DeadlineAt) {
			row.applyOutcome(domain.ProbePreflightOutcome{State: domain.ProbePreflightInterrupted, ErrorCode: "interrupted"}, preflightStamp(row.UpdatedAt, now))
		} else {
			if current.State == domain.ProbePreflightCancelRequested {
				out = &current
				return nil
			}
			row.State = domain.ProbePreflightCancelRequested
			row.UpdatedAt = preflightStamp(row.UpdatedAt, now)
		}
		if _, err := tx.NewUpdate().Model(&row).
			Column("state", "error_code", "updated_at").WherePK().Exec(ctx); err != nil {
			return err
		}
		op, err := row.operation()
		if err != nil {
			return err
		}
		out = &op
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
