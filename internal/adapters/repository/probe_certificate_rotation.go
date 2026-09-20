package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxProbeCertificateRotations = 1024

var _ ports.ProbeCertificateRotationRepository = (*ProbeCommandStore)(nil)

type probeCertificateRotationRow struct {
	bun.BaseModel                                `bun:"table:probe_certificate_rotations"`
	RotationID                                   string `bun:"rotation_id,pk"`
	HubID, ProbeID, StreamID, EnrollmentID       string
	CredentialVersion                            int64
	Endpoint, PreviousFingerprint                string
	CertificateVersion, PreviousVersion          int64
	ValidForDays                                 int
	PrepareCommandID, ActivateCommandID          string
	CreatedAt, OverlapExpiresAt                  time.Time
	OverlapClosed                                bool
	State                                        string
	PreparedAt, ActivatedAt, CertificateNotAfter *time.Time
	Fingerprint                                  string `bun:",nullzero"`
	FailureCode                                  string
	UpdatedAt, RetainUntil                       time.Time
}

func (r probeCertificateRotationRow) metadata() domain.ProbeCertificateRotation {
	return domain.ProbeCertificateRotation{RotationID: r.RotationID, Current: domain.ProbeCredentialMetadata{HubID: r.HubID, ProbeID: r.ProbeID, StreamID: r.StreamID, EnrollmentID: r.EnrollmentID, CredentialVersion: r.CredentialVersion, Endpoint: r.Endpoint, Fingerprint: r.PreviousFingerprint}, CertificateVersion: r.CertificateVersion, PreviousVersion: r.PreviousVersion, ValidForDays: r.ValidForDays, PrepareCommandID: r.PrepareCommandID, ActivateCommandID: r.ActivateCommandID, CreatedAt: r.CreatedAt.UTC(), OverlapExpiresAt: r.OverlapExpiresAt.UTC(), OverlapClosed: r.OverlapClosed, State: r.State, PreparedAt: utcTimePtr(r.PreparedAt), ActivatedAt: utcTimePtr(r.ActivatedAt), CertificateNotAfter: utcTimePtr(r.CertificateNotAfter), Fingerprint: r.Fingerprint, FailureCode: r.FailureCode, UpdatedAt: r.UpdatedAt.UTC()}
}

// CreateCertificateRotation commits preparation and reserves its activation ID
// and bounded capacity. No activation payload exists until a source pin is known.
func (s *ProbeCommandStore) CreateCertificateRotation(ctx context.Context, c domain.ProtectedProbeCertificateRotation) (*domain.ProbeCertificateRotation, error) {
	if s == nil || s.db == nil || s.protector == nil || s.credentials == nil || s.certificateCodec == nil || c.State != "preparing" || c.PreparedAt != nil || c.ActivatedAt != nil || c.CertificateNotAfter != nil || c.Fingerprint != "" || c.FailureCode != "" || c.OverlapClosed {
		return nil, domain.ErrValidation
	}
	if err := s.verifyCertificatePreparation(ctx, c.ProbeCertificateRotation, c.PrepareCommand); err != nil {
		return nil, err
	}
	var out *domain.ProbeCertificateRotation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		m := c.Current
		if err := s.lockCommandTarget(ctx, tx, m.HubID, m.ProbeID, m.StreamID); err != nil {
			return err
		}
		var existing probeCertificateRotationRow
		err := tx.NewSelect().Model(&existing).Where("rotation_id = ?", c.RotationID).Scan(ctx)
		if err == nil {
			if !sameCertificateIssuance(existing.metadata(), c.ProbeCertificateRotation) {
				return ports.ErrConflict
			}
			var saved probeCommandRow
			if err := tx.NewSelect().Model(&saved).Where("command_id = ?", c.PrepareCommandID).Scan(ctx); err != nil {
				return err
			}
			if !sameCommandMetadata(saved.metadata(), c.PrepareCommand.ProbeCommandMetadata) {
				return ports.ErrConflict
			}
			r := existing.metadata()
			out = &r
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(c.OverlapExpiresAt) || c.CreatedAt.After(now.Add(30*time.Second)) {
			return domain.ErrValidation
		}
		var current probeConnectionRow
		if err := tx.NewSelect().Model(&current).Where("probe_id = ?", m.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if current.connection().ProbeCredentialMetadata != m || current.State != "active" || current.CertificateVersion != c.PreviousVersion || c.CertificateVersion <= current.CertificateHighWater {
			return ports.ErrConflict
		}
		if _, err := s.credentials.OpenCredential(ctx, m, current.ProtectedCredential); err != nil {
			return err
		}
		for _, table := range []string{"probe_credential_rotations", "probe_certificate_rotations"} {
			var count int
			if err := tx.NewRaw("SELECT COUNT(*) FROM "+table+" WHERE probe_id = ? AND (state IN ('preparing', 'activating') OR overlap_expires_at > ?)", m.ProbeID, now).Scan(ctx, &count); err != nil {
				return err
			}
			if count > 0 {
				return ports.ErrConflict
			}
		}
		if _, err := tx.NewDelete().Model((*probeCertificateRotationRow)(nil)).Where("probe_id = ? AND state IN (?, ?) AND retain_until < ?", m.ProbeID, "active", "failed", now).Exec(ctx); err != nil {
			return err
		}
		count, err := tx.NewSelect().Model((*probeCertificateRotationRow)(nil)).Where("probe_id = ?", m.ProbeID).Count(ctx)
		if err != nil {
			return err
		}
		if count >= maxProbeCertificateRotations {
			return ports.ErrConflict
		}
		if err := reserveCommandCapacity(ctx, tx, m.ProbeID, now, 2, len(c.PrepareCommand.ProtectedPayload)+domain.MaxProbeCommandBytes+domain.ProbeConfigProtectionOverhead); err != nil {
			return err
		}
		command := newPendingCommandRow(c.PrepareCommand, now)
		if _, err := tx.NewInsert().Model(&command).Exec(ctx); err != nil {
			return err
		}
		// Reserve the global command primary key with a non-dispatchable blocked
		// row. No payload/digest or success is invented before the source pin exists.
		activation := probeCommandRow{CommandID: c.ActivateCommandID, HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID, Kind: "certificate.activate", CreatedAt: c.CreatedAt.UTC(), ExpiresAt: c.OverlapExpiresAt.UTC(), Status: "blocked", RetainUntil: command.RetainUntil, UpdatedAt: now}
		if _, err := tx.NewInsert().Model(&activation).Exec(ctx); err != nil {
			return err
		}
		row := probeCertificateRotationRow{RotationID: c.RotationID, HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID, EnrollmentID: m.EnrollmentID, CredentialVersion: m.CredentialVersion, Endpoint: m.Endpoint, PreviousFingerprint: m.Fingerprint, CertificateVersion: c.CertificateVersion, PreviousVersion: c.PreviousVersion, ValidForDays: c.ValidForDays, PrepareCommandID: c.PrepareCommandID, ActivateCommandID: c.ActivateCommandID, CreatedAt: c.CreatedAt.UTC(), OverlapExpiresAt: c.OverlapExpiresAt.UTC(), State: "preparing", UpdatedAt: now, RetainUntil: c.OverlapExpiresAt.Add(probeCommandRetention)}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(&current).Set("certificate_high_water = ?", c.CertificateVersion).WherePK().Exec(ctx); err != nil {
			return err
		}
		r := row.metadata()
		out = &r
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	return out, nil
}

// GetCertificateRotation exposes only stable nonsecret operation metadata.
func (s *ProbeCommandStore) GetCertificateRotation(ctx context.Context, hubID, probeID, rotationID string) (*domain.ProbeCertificateRotation, error) {
	if !domain.ValidHubID(hubID) || !domain.ValidHubID(probeID) || !domain.ValidHubID(rotationID) {
		return nil, domain.ErrValidation
	}
	var row probeCertificateRotationRow
	if err := s.db.NewSelect().Model(&row).Where("hub_id = ? AND probe_id = ? AND rotation_id = ?", hubID, probeID, rotationID).Scan(ctx); err != nil {
		return nil, probeCommandError(ctx, err)
	}
	r := row.metadata()
	return &r, nil
}

func (s *ProbeCommandStore) verifyCertificatePreparation(ctx context.Context, r domain.ProbeCertificateRotation, prepare domain.ProtectedProbeCommand) error {
	m := r.Current
	if !domain.ValidProbeCredentialMetadata(m) || !domain.ValidHubID(r.RotationID) || !domain.ValidHubID(r.PrepareCommandID) || !domain.ValidHubID(r.ActivateCommandID) || r.RotationID == r.PrepareCommandID || r.RotationID == r.ActivateCommandID || r.PrepareCommandID == r.ActivateCommandID || r.PreviousVersion < 1 || r.CertificateVersion <= r.PreviousVersion || r.ValidForDays < 1 || r.ValidForDays > 3650 || r.CreatedAt.IsZero() || r.CreatedAt.Nanosecond()%1000 != 0 || !r.OverlapExpiresAt.Equal(r.CreatedAt.Add(domain.ProbeCredentialOverlap)) {
		return domain.ErrValidation
	}
	meta := prepare.ProbeCommandMetadata
	if !domain.ValidProbeCommandMetadata(meta) || meta.CommandID != r.PrepareCommandID || meta.Kind != "certificate.prepare" || meta.HubID != m.HubID || meta.ProbeID != m.ProbeID || meta.StreamID != m.StreamID || !meta.CreatedAt.Equal(r.CreatedAt) || !meta.ExpiresAt.Equal(r.OverlapExpiresAt) {
		return domain.ErrValidation
	}
	plain, err := s.protector.OpenCommand(ctx, meta, prepare.ProtectedPayload)
	if err != nil {
		return err
	}
	defer clear(plain)
	c, err := s.certificateCodec.DecodeCertificateCommand(ctx, plain)
	if err != nil || !certificateMatchesMetadata(c, meta) || c.RotationID != r.RotationID || c.CertificateVersion != r.CertificateVersion || c.ValidForDays != r.ValidForDays {
		return domain.ErrValidation
	}
	return nil
}

func certificateMatchesMetadata(c domain.ProbeCertificateCommand, m domain.ProbeCommandMetadata) bool {
	return c.CommandID == m.CommandID && c.ProbeID == m.ProbeID && c.Kind == m.Kind && m.SourceAlertID == nil && m.AssignmentGeneration == nil && c.CreatedAt.Equal(m.CreatedAt) && c.ExpiresAt.Equal(m.ExpiresAt) && hex.EncodeToString(c.PayloadHash[:]) == m.PayloadSHA256
}

func sameCertificateIssuance(a, b domain.ProbeCertificateRotation) bool {
	return a.RotationID == b.RotationID && a.Current == b.Current && a.CertificateVersion == b.CertificateVersion && a.PreviousVersion == b.PreviousVersion && a.ValidForDays == b.ValidForDays && a.PrepareCommandID == b.PrepareCommandID && a.ActivateCommandID == b.ActivateCommandID && a.CreatedAt.Equal(b.CreatedAt) && a.OverlapExpiresAt.Equal(b.OverlapExpiresAt)
}
