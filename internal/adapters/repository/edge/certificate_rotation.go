package edge

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"strconv"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxCertificateRotations = 1024

var _ ports.EdgeCertificateRepository = (*Store)(nil)

type edgeCertificateRotation struct {
	bun.BaseModel                         `bun:"table:edge_certificate_rotations"`
	RotationID                            string `bun:"rotation_id,pk"`
	Version                               int64
	HubID, ProbeID, StreamID, Fingerprint string
	CreatedAt, NotBefore, NotAfter        int64
	ProtectedPEM                          []byte `bun:"protected_pem"`
	ValidForDays                          int
	OverlapExpiresAt, PreparedAt          int64
	ActivatedAt                           *int64
	OverlapClosed                         bool
	PreviousVersion, RetainUntil          int64
}

func (r edgeCertificateRotation) protected() domain.ProtectedEdgeCertificate {
	return domain.ProtectedEdgeCertificate{EdgeCertificateMetadata: domain.EdgeCertificateMetadata{HubID: r.HubID, ProbeID: r.ProbeID, StreamID: r.StreamID, RotationID: r.RotationID, Version: r.Version, Fingerprint: r.Fingerprint, CreatedAt: time.UnixMicro(r.CreatedAt).UTC(), NotBefore: time.UnixMicro(r.NotBefore).UTC(), NotAfter: time.UnixMicro(r.NotAfter).UTC()}, ProtectedPEM: r.ProtectedPEM}
}

// ApplyCertificateCommand commits protected preparation or active selection and
// the immutable receipt together. No success is returned before the final write.
func (s *Store) ApplyCertificateCommand(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand) (domain.ProbeCommandOutcome, error) {
	if s.certificates == nil || !domain.ValidHubID(a.HubID) || !domain.ValidHubID(a.ProbeID) || !domain.ValidHubID(a.StreamID) || a.ConnectionGeneration <= 0 || !domain.ValidProbeCertificateCommand(c) {
		return domain.ProbeCommandOutcome{}, domain.ErrValidation
	}
	return s.applyCommand(ctx, a, c.CommandID, c.ProbeID, c.Kind, certificateCommandHash(a, c), c.CreatedAt, c.ExpiresAt,
		func(ctx context.Context, tx bun.Tx, _ domain.EdgeIdentity, now time.Time, result *edgeCommandRow) error {
			if c.Kind == "certificate.prepare" {
				return s.prepareCertificate(ctx, tx, a, c, now, result)
			}
			return s.activateCertificate(ctx, tx, c, now, result)
		})
}

func (s *Store) prepareCertificate(ctx context.Context, tx bun.Tx, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand, now time.Time, result *edgeCommandRow) error {
	var row edgeCertificateRotation
	err := tx.NewSelect().Model(&row).Where("rotation_id = ?", c.RotationID).Scan(ctx)
	if err == nil {
		if row.Version != c.CertificateVersion || row.ValidForDays != c.ValidForDays || row.CreatedAt != c.CreatedAt.UnixMicro() {
			rejectCredential(result, "rotation_conflict", "Rotation identity conflicts with retained preparation")
			return nil
		}
		certificatePreparedResult(result, row, "already_applied")
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	deadline := c.CreatedAt.Add(domain.ProbeCredentialOverlap)
	if !now.Before(deadline) {
		rejectCredential(result, "overlap_expired", "Certificate overlap expired before preparation")
		return nil
	}
	state, err := readCertificateState(ctx, tx)
	if err != nil {
		return err
	}
	if c.CertificateVersion <= state.HighestVersion {
		rejectCredential(result, "rotation_conflict", "Certificate version is not new")
		return nil
	}
	for _, table := range []string{"edge_certificate_rotations", "edge_credential_rotations"} {
		var count int
		if err := tx.NewRaw("SELECT COUNT(*) FROM "+table+" WHERE overlap_closed = ? AND overlap_expires_at > ?", false, now.UnixMicro()).Scan(ctx, &count); err != nil {
			return err
		}
		if count > 0 {
			rejectCredential(result, "rotation_in_progress", "Another identity overlap is still active")
			return nil
		}
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM edge_certificate_rotations WHERE retain_until < ? AND version <> ?", now.UnixMicro(), state.ActiveVersion); err != nil {
		return err
	}
	count, err := tx.NewSelect().Model((*edgeCertificateRotation)(nil)).Count(ctx)
	if err != nil {
		return err
	}
	if count >= maxCertificateRotations {
		return ErrQueueFull
	}
	material, err := s.certificates.PrepareCertificate(ctx, a, c, now)
	if err != nil {
		return err
	}
	m := material.EdgeCertificateMetadata
	if !domain.ValidEdgeCertificateMetadata(m) || m.HubID != a.HubID || m.ProbeID != a.ProbeID || m.StreamID != a.StreamID || m.RotationID != c.RotationID || m.Version != c.CertificateVersion || !m.CreatedAt.Equal(c.CreatedAt) || !m.NotBefore.Equal(now.Truncate(time.Second).Add(-5*time.Minute)) || !m.NotAfter.Equal(now.Truncate(time.Second).Add(time.Duration(c.ValidForDays)*24*time.Hour)) || len(material.ProtectedPEM) <= domain.ProbeConfigProtectionOverhead || len(material.ProtectedPEM) > domain.MaxEdgeCertificateBytes+domain.ProbeConfigProtectionOverhead {
		return domain.ErrValidation
	}
	if err := s.certificates.ValidateCertificate(ctx, material, now); err != nil {
		return err
	}
	row = edgeCertificateRotation{RotationID: m.RotationID, Version: m.Version, HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID, Fingerprint: m.Fingerprint, CreatedAt: m.CreatedAt.UnixMicro(), NotBefore: m.NotBefore.UnixMicro(), NotAfter: m.NotAfter.UnixMicro(), ProtectedPEM: material.ProtectedPEM, ValidForDays: c.ValidForDays, OverlapExpiresAt: deadline.UnixMicro(), PreparedAt: now.UnixMicro(), PreviousVersion: state.ActiveVersion, RetainUntil: c.ExpiresAt.Add(commandRetention).UnixMicro()}
	if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE edge_certificate_state SET highest_version = ? WHERE id = 1", row.Version); err != nil {
		return err
	}
	certificatePreparedResult(result, row, "applied")
	return nil
}

func certificatePreparedResult(result *edgeCommandRow, row edgeCertificateRotation, status string) {
	result.Status, result.Message, result.AppliedAt = status, "Certificate prepared", &row.PreparedAt
	result.CertificateVersion, result.CertificateFingerprint, result.CertificateNotAfter = &row.Version, &row.Fingerprint, &row.NotAfter
}

func (s *Store) activateCertificate(ctx context.Context, tx bun.Tx, c domain.ProbeCertificateCommand, now time.Time, result *edgeCommandRow) error {
	var row edgeCertificateRotation
	err := tx.NewSelect().Model(&row).Where("rotation_id = ? AND version = ?", c.RotationID, c.CertificateVersion).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		rejectCredential(result, "rotation_not_found", "Matching certificate preparation was not found")
		return nil
	}
	if err != nil {
		return err
	}
	if c.ExpectedFingerprint != row.Fingerprint {
		rejectCredential(result, "rotation_conflict", "Certificate activation pin does not match preparation")
		return nil
	}
	if row.ActivatedAt != nil {
		result.Status, result.Message, result.AppliedAt = "already_applied", "Certificate was already activated", row.ActivatedAt
		return nil
	}
	if row.OverlapClosed || now.UnixMicro() >= row.OverlapExpiresAt || now.UnixMicro() < row.PreparedAt {
		rejectCredential(result, "overlap_expired", "Certificate overlap is no longer available")
		return nil
	}
	state, err := readCertificateState(ctx, tx)
	if err != nil {
		return err
	}
	if state.ActiveVersion != row.PreviousVersion || state.HighestVersion != row.Version {
		rejectCredential(result, "rotation_conflict", "Certificate preparation no longer selects the active identity")
		return nil
	}
	if err := s.certificates.ValidateCertificate(ctx, row.protected(), now); err != nil {
		return err
	}
	at := now.UnixMicro()
	if _, err := tx.ExecContext(ctx, "UPDATE edge_certificate_state SET active_version = ? WHERE id = 1", row.Version); err != nil {
		return err
	}
	if _, err := tx.NewUpdate().Model(&row).Set("activated_at = ?", at).WherePK().Exec(ctx); err != nil {
		return err
	}
	result.Status, result.Message, result.AppliedAt = "applied", "Certificate activated", &at
	return nil
}

func readCertificateState(ctx context.Context, db bun.IDB) (domain.EdgeCertificateState, error) {
	var state domain.EdgeCertificateState
	err := db.NewRaw("SELECT active_version, highest_version FROM edge_certificate_state WHERE id = 1").Scan(ctx, &state)
	if err != nil {
		return state, err
	}
	if state.ActiveVersion < 1 || state.HighestVersion < state.ActiveVersion {
		return state, ErrStorage
	}
	return state, nil
}

// ReadActiveCertificate reads pointer and material from one coherent transaction.
// The TLS owner must authenticate the returned material before publishing it.
func (s *Store) ReadActiveCertificate(ctx context.Context) (domain.EdgeCertificateState, error) {
	var state domain.EdgeCertificateState
	err := s.write(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		if err := expireCertificateOverlaps(ctx, tx, s.commandNow().UTC()); err != nil {
			return err
		}
		var err error
		state, err = readCertificateState(ctx, tx)
		if err != nil {
			return err
		}
		state.ProbeID, state.StreamID = identity.ProbeID, identity.StreamID
		if err := tx.NewRaw("SELECT initial_stream_id FROM edge_identity WHERE id = 1").Scan(ctx, &state.InitialStreamID); err != nil {
			return err
		}
		if state.ActiveVersion == 1 {
			return nil
		}
		var row edgeCertificateRotation
		if err := tx.NewSelect().Model(&row).Where("version = ?", state.ActiveVersion).Scan(ctx); err != nil {
			return ErrStorage
		}
		if row.ActivatedAt == nil || row.HubID != identity.HubID || row.ProbeID != identity.ProbeID || row.StreamID != identity.StreamID {
			return ErrStorage
		}
		certificate := row.protected()
		if !domain.ValidEdgeCertificateMetadata(certificate.EdgeCertificateMetadata) {
			return ErrStorage
		}
		state.Certificate = &certificate
		return nil
	})
	if err != nil {
		return domain.EdgeCertificateState{}, err
	}
	return state, nil
}

func expireCertificateOverlaps(ctx context.Context, tx bun.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, "UPDATE edge_certificate_rotations SET overlap_closed = ? WHERE overlap_closed = ? AND overlap_expires_at <= ?", true, false, now.UnixMicro())
	return err
}

func certificateCommandHash(a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand) [32]byte {
	b := append([]byte("phoenix.certificate.command.v1"), c.PayloadHash[:]...)
	for _, value := range []string{a.HubID, a.ProbeID, a.StreamID, c.CommandID, c.ProbeID, c.Kind, c.RotationID, strconv.FormatInt(c.CertificateVersion, 10), strconv.Itoa(c.ValidForDays), c.ExpectedFingerprint, c.CreatedAt.UTC().Format(time.RFC3339Nano), c.ExpiresAt.UTC().Format(time.RFC3339Nano)} {
		b = binary.BigEndian.AppendUint64(b, uint64(len(value)))
		b = append(b, value...)
	}
	return sha256.Sum256(b)
}
