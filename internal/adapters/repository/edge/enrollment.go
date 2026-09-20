package edge

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// IssueEnrollmentToken atomically replaces an unconsumed local token. Once bound,
// this path cannot reset identity or generate another enrollment authorization.
func (s *Store) IssueEnrollmentToken(ctx context.Context, token domain.EdgeEnrollmentToken) error {
	if token.Hash == [32]byte{} || token.IssuedAt.IsZero() || !token.ExpiresAt.After(token.IssuedAt) || token.ExpiresAt.Sub(token.IssuedAt) > 10*time.Minute {
		return domain.ErrValidation
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if i.HubID != "" {
			return ports.ErrConflict
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO edge_credentials (kind, token_hash, issued_at, expires_at) VALUES ('enrollment', ?, ?, ?)
		 ON CONFLICT (kind) DO UPDATE SET token_hash = excluded.token_hash, issued_at = excluded.issued_at, expires_at = excluded.expires_at`, token.Hash[:], token.IssuedAt.UTC().UnixMicro(), token.ExpiresAt.UTC().UnixMicro())
		return err
	})
}

func enrollmentTokenValid(ctx context.Context, db bun.IDB, hash [32]byte, at time.Time) (bool, error) {
	if hash == [32]byte{} || at.IsZero() {
		return false, domain.ErrValidation
	}
	var row struct {
		TokenHash []byte
		IssuedAt  int64
		ExpiresAt int64
	}
	err := db.NewRaw("SELECT token_hash, issued_at, expires_at FROM edge_credentials WHERE kind = 'enrollment'").Scan(ctx, &row)
	if errors.Is(storageError(ctx, err), ports.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, storageError(ctx, err)
	}
	now := at.UTC().UnixMicro()
	return subtle.ConstantTimeCompare(hash[:], row.TokenHash) == 1 && now >= row.IssuedAt && now < row.ExpiresAt, nil
}

// EnrollmentTokenValid authorizes an upgrade only; CommitEnrollment rechecks
// inside its writer transaction before consuming the token.
func (s *Store) EnrollmentTokenValid(ctx context.Context, hash [32]byte, at time.Time) (bool, error) {
	return enrollmentTokenValid(ctx, s.db, hash, at)
}

// CommitEnrollment commits binding, credential hash and token consumption together.
func (s *Store) CommitEnrollment(ctx context.Context, hash [32]byte, binding domain.EdgeEnrollment) error {
	if hash == [32]byte{} || !domain.ValidEdgeEnrollment(binding) {
		return domain.ErrValidation
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if i.HubID != "" || binding.ProbeID != i.ProbeID {
			return ports.ErrConflict
		}
		valid, err := enrollmentTokenValid(ctx, tx, hash, binding.AppliedAt)
		if err != nil {
			return err
		}
		if !valid {
			return ports.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO edge_credentials (kind, token_hash, hub_id, enrollment_id, version, issued_at) VALUES ('runtime', ?, ?, ?, ?, ?)", binding.TokenHash[:], binding.HubID, binding.EnrollmentID, binding.CredentialVersion, binding.AppliedAt.UTC().UnixMicro()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET hub_id = ? WHERE id = 1", binding.HubID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM edge_credentials WHERE kind = 'enrollment'")
		return err
	})
}

// ReadEnrollment returns the only active binding without any plaintext credential.
func (s *Store) ReadEnrollment(ctx context.Context) (domain.EdgeEnrollment, error) {
	var row struct {
		ProbeID      string
		HubID        string
		EnrollmentID string
		Version      int64
		TokenHash    []byte
		IssuedAt     int64
	}
	err := s.db.NewRaw(`SELECT i.probe_id, c.hub_id, c.enrollment_id, c.version, c.token_hash, c.issued_at
	 FROM edge_credentials c JOIN edge_identity i ON i.id = 1 AND i.hub_id = c.hub_id WHERE c.kind = 'runtime'`).Scan(ctx, &row)
	if err != nil {
		return domain.EdgeEnrollment{}, storageError(ctx, err)
	}
	result := domain.EdgeEnrollment{ProbeID: row.ProbeID, HubID: row.HubID, EnrollmentID: row.EnrollmentID, CredentialVersion: row.Version, AppliedAt: time.UnixMicro(row.IssuedAt).UTC()}
	if len(row.TokenHash) != len(result.TokenHash) {
		return domain.EdgeEnrollment{}, ErrStorage
	}
	copy(result.TokenHash[:], row.TokenHash)
	if !domain.ValidEdgeEnrollment(result) {
		return domain.EdgeEnrollment{}, ErrStorage
	}
	return result, nil
}
