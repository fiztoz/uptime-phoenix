package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxCredentialRotations = 1024

var _ ports.EdgeCredentialRepository = (*Store)(nil)

type edgeCredentialRotation struct {
	bun.BaseModel     `bun:"table:edge_credential_rotations"`
	RotationID        string `bun:"rotation_id,pk"`
	Version           int64
	TokenHash         []byte
	OverlapExpiresAt  int64
	PreparedAt        int64
	ActivatedAt       *int64
	OverlapClosed     bool
	PreviousTokenHash []byte
	PreviousVersion   int64
	PreviousIssuedAt  int64
	RetainUntil       int64
}

// ApplyCredentialCommand atomically persists prepare/activate and its immutable
// receipt. No plaintext token or unbounded request body is stored.
func (s *Store) ApplyCredentialCommand(ctx context.Context, authority domain.EdgeCommandAuthority, c domain.ProbeCredentialCommand) (domain.ProbeCommandOutcome, error) {
	if !validCredentialCommand(authority, c) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	return s.applyCommand(ctx, authority, c.CommandID, c.ProbeID, c.Kind, credentialCommandHash(authority, c), c.CreatedAt, c.ExpiresAt,
		func(ctx context.Context, tx bun.Tx, _ domain.EdgeIdentity, now time.Time, result *edgeCommandRow) error {
			if c.Kind == "credential.prepare" {
				return prepareCredential(ctx, tx, c, now, result)
			}
			return activateCredential(ctx, tx, c, now, result)
		})
}

func prepareCredential(ctx context.Context, tx bun.Tx, c domain.ProbeCredentialCommand, now time.Time, result *edgeCommandRow) error {
	var existing edgeCredentialRotation
	err := tx.NewSelect().Model(&existing).Where("rotation_id = ?", c.RotationID).Scan(ctx)
	if err == nil {
		if existing.Version != c.CredentialVersion || !bytes.Equal(existing.TokenHash, c.TokenHash[:]) || existing.OverlapExpiresAt != c.OverlapExpiresAt.UnixMicro() {
			rejectCredential(result, "rotation_conflict", "Rotation identity conflicts with retained preparation")
			return nil
		}
		result.Status, result.Message, result.AppliedAt, result.CredentialVersion = "already_applied", "Credential was already prepared", &existing.PreparedAt, &existing.Version
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if !now.Before(c.OverlapExpiresAt) {
		rejectCredential(result, "overlap_expired", "Credential overlap expired before preparation")
		return nil
	}
	current, err := readEnrollment(ctx, tx)
	if err != nil {
		return err
	}
	var highest int64
	if err := tx.NewRaw("SELECT highest_version FROM edge_credential_state WHERE id = 1").Scan(ctx, &highest); err != nil {
		return err
	}
	if c.CredentialVersion <= highest || c.CredentialVersion <= current.CredentialVersion || subtle.ConstantTimeCompare(c.TokenHash[:], current.TokenHash[:]) == 1 {
		rejectCredential(result, "rotation_conflict", "Credential version or digest is not new")
		return nil
	}
	active, err := tx.NewSelect().Model((*edgeCredentialRotation)(nil)).Where("overlap_closed = ? AND overlap_expires_at > ?", false, now.UnixMicro()).Exists(ctx)
	if err != nil {
		return err
	}
	if active {
		rejectCredential(result, "rotation_in_progress", "Another credential overlap is still active")
		return nil
	}
	active, err = tx.NewSelect().Model((*edgeCertificateRotation)(nil)).Where("overlap_closed = ? AND overlap_expires_at > ?", false, now.UnixMicro()).Exists(ctx)
	if err != nil {
		return err
	}
	if active {
		rejectCredential(result, "rotation_in_progress", "Another certificate overlap is still active")
		return nil
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM edge_credential_rotations WHERE retain_until < ? AND version < ?", now.UnixMicro(), highest); err != nil {
		return err
	}
	count, err := tx.NewSelect().Model((*edgeCredentialRotation)(nil)).Count(ctx)
	if err != nil {
		return err
	}
	if count >= maxCredentialRotations {
		return ErrQueueFull
	}
	row := edgeCredentialRotation{RotationID: c.RotationID, Version: c.CredentialVersion, TokenHash: c.TokenHash[:], OverlapExpiresAt: c.OverlapExpiresAt.UnixMicro(), PreparedAt: now.UnixMicro(), PreviousTokenHash: current.TokenHash[:], PreviousVersion: current.CredentialVersion, PreviousIssuedAt: current.AppliedAt.UnixMicro(), RetainUntil: c.ExpiresAt.Add(commandRetention).UnixMicro()}
	if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE edge_credential_state SET highest_version = ? WHERE id = 1", row.Version); err != nil {
		return err
	}
	result.Status, result.Message, result.AppliedAt, result.CredentialVersion = "applied", "Credential prepared", &row.PreparedAt, &row.Version
	return nil
}

func activateCredential(ctx context.Context, tx bun.Tx, c domain.ProbeCredentialCommand, now time.Time, result *edgeCommandRow) error {
	var row edgeCredentialRotation
	err := tx.NewSelect().Model(&row).Where("rotation_id = ? AND version = ?", c.RotationID, c.CredentialVersion).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		rejectCredential(result, "rotation_not_found", "Matching credential preparation was not found")
		return nil
	}
	if err != nil {
		return err
	}
	if row.ActivatedAt != nil {
		result.Status, result.Message, result.AppliedAt = "already_applied", "Credential was already activated", row.ActivatedAt
		return nil
	}
	if row.OverlapClosed || now.UnixMicro() >= row.OverlapExpiresAt || now.UnixMicro() < row.PreparedAt {
		rejectCredential(result, "overlap_expired", "Credential overlap is no longer available")
		return nil
	}
	var highest int64
	if err := tx.NewRaw("SELECT highest_version FROM edge_credential_state WHERE id = 1").Scan(ctx, &highest); err != nil {
		return err
	}
	current, err := readEnrollment(ctx, tx)
	if err != nil {
		return err
	}
	if highest != row.Version || current.CredentialVersion != row.PreviousVersion || !bytes.Equal(current.TokenHash[:], row.PreviousTokenHash) {
		rejectCredential(result, "rotation_conflict", "Credential preparation no longer selects the active identity")
		return nil
	}
	at := now.UnixMicro()
	if _, err := tx.ExecContext(ctx, "UPDATE edge_credentials SET token_hash = ?, version = ?, issued_at = ? WHERE kind = 'runtime'", row.TokenHash, row.Version, at); err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model(&row).Set("activated_at = ?", at).WherePK().Exec(ctx); err != nil {
		return err
	}
	result.Status, result.Message, result.AppliedAt = "applied", "Credential activated", &at
	return nil
}

func rejectCredential(result *edgeCommandRow, code, message string) {
	result.Status, result.Code, result.Message = "rejected", code, message
}

// ReadRuntimeCredentials returns at most two trusted bindings. Observing expiry
// permanently closes that overlap, so a later wall-clock rollback cannot revive it.
func (s *Store) ReadRuntimeCredentials(ctx context.Context) ([]domain.EdgeEnrollment, error) {
	var bindings []domain.EdgeEnrollment
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, _ domain.EdgeIdentity) error {
		var err error
		bindings, err = runtimeCredentials(ctx, tx, s.commandNow().UTC())
		return err
	})
	if err != nil {
		return nil, err
	}
	return bindings, nil
}

func runtimeCredentials(ctx context.Context, tx bun.Tx, now time.Time) ([]domain.EdgeEnrollment, error) {
	current, err := readEnrollment(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := expireCredentialOverlaps(ctx, tx, now); err != nil {
		return nil, err
	}
	var row edgeCredentialRotation
	err = tx.NewSelect().Model(&row).Where("version = (SELECT highest_version FROM edge_credential_state WHERE id = 1)").Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (row.OverlapClosed || now.UnixMicro() < row.PreparedAt) {
		return []domain.EdgeEnrollment{current}, nil
	}
	if err != nil {
		return nil, err
	}
	deadline := time.UnixMicro(row.OverlapExpiresAt).UTC()
	other := current
	other.ValidUntil = &deadline
	if row.ActivatedAt == nil {
		if current.CredentialVersion != row.PreviousVersion || !bytes.Equal(current.TokenHash[:], row.PreviousTokenHash) {
			return nil, ErrStorage
		}
		// Even the active token's socket reconnects at this deadline. If activation
		// never occurs it remains the current credential on the following admission.
		current.ValidUntil = &deadline
		other.CredentialVersion, other.AppliedAt = row.Version, time.UnixMicro(row.PreparedAt).UTC()
		copy(other.TokenHash[:], row.TokenHash)
	} else {
		if current.CredentialVersion != row.Version || !bytes.Equal(current.TokenHash[:], row.TokenHash) {
			return nil, ErrStorage
		}
		other.CredentialVersion, other.AppliedAt = row.PreviousVersion, time.UnixMicro(row.PreviousIssuedAt).UTC()
		copy(other.TokenHash[:], row.PreviousTokenHash)
	}
	return []domain.EdgeEnrollment{current, other}, nil
}

func expireCredentialOverlaps(ctx context.Context, tx bun.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE edge_credential_rotations SET overlap_closed = ? WHERE overlap_closed = ? AND overlap_expires_at <= ?", true, false, now.UnixMicro())
	return err
}

// AcceptCredentialConnection closes the header-authentication/admission race.
// The returned binding, not a cached header result, controls the socket deadline.
func (s *Store) AcceptCredentialConnection(ctx context.Context, binding domain.EdgeEnrollment, generation int64) (domain.EdgeEnrollment, error) {
	if !domain.ValidEdgeEnrollment(binding) || generation <= 0 {
		return domain.EdgeEnrollment{}, domain.ErrValidation
	}
	var admitted domain.EdgeEnrollment
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if binding.HubID != identity.HubID || binding.ProbeID != identity.ProbeID || generation <= identity.ConnectionGeneration {
			return ports.ErrConflict
		}
		now := s.commandNow().UTC()
		bindings, err := runtimeCredentials(ctx, tx, now)
		if err != nil {
			return err
		}
		certificateDeadline, eligible, err := certificateAdmission(ctx, tx, identity, binding, now)
		if err != nil {
			return err
		}
		if !eligible {
			return nil
		}
		for _, candidate := range bindings {
			if candidate.CredentialVersion == binding.CredentialVersion && candidate.EnrollmentID == binding.EnrollmentID && subtle.ConstantTimeCompare(candidate.TokenHash[:], binding.TokenHash[:]) == 1 {
				admitted = candidate
			}
		}
		if admitted.CredentialVersion == 0 {
			// Commit the irreversible expiry observation without admitting a socket.
			return nil
		}
		admitted.CertificateFingerprint = binding.CertificateFingerprint
		admitted.CertificateNotBefore = binding.CertificateNotBefore
		admitted.CertificateNotAfter = binding.CertificateNotAfter
		if certificateDeadline != nil && (admitted.ValidUntil == nil || certificateDeadline.Before(*admitted.ValidUntil)) {
			admitted.ValidUntil = certificateDeadline
		}
		_, err = tx.ExecContext(ctx, "UPDATE edge_identity SET connection_generation = ? WHERE id = 1", generation)
		return err
	})
	if err != nil {
		return domain.EdgeEnrollment{}, err
	}
	if admitted.CredentialVersion == 0 {
		return domain.EdgeEnrollment{}, ports.ErrConflict
	}
	return admitted, nil
}

func validCredentialCommand(a domain.EdgeCommandAuthority, c domain.ProbeCredentialCommand) bool {
	return domain.ValidHubID(a.HubID) && domain.ValidHubID(a.ProbeID) && domain.ValidHubID(a.StreamID) && a.ConnectionGeneration > 0 && domain.ValidProbeCredentialCommand(c)
}

func credentialCommandHash(a domain.EdgeCommandAuthority, c domain.ProbeCredentialCommand) [32]byte {
	b := append([]byte("phoenix.credential.command.v1"), c.PayloadHash[:]...)
	b = append(b, c.TokenHash[:]...)
	for _, value := range []string{a.HubID, a.ProbeID, a.StreamID, c.CommandID, c.ProbeID, c.Kind, c.RotationID, strconv.FormatInt(c.CredentialVersion, 10), c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ExpiresAt.UTC().Format(time.RFC3339Nano), c.OverlapExpiresAt.UTC().Format(time.RFC3339Nano)} {
		b = binary.BigEndian.AppendUint64(b, uint64(len(value)))
		b = append(b, value...)
	}
	return sha256.Sum256(b)
}
