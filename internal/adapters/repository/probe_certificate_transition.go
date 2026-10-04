package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func (s *ProbeCommandStore) storedCertificateCommands(ctx context.Context, tx bun.Tx, row probeCertificateRotationRow) error {
	if s.certificateCodec == nil || s.credentials == nil {
		return domain.ErrValidation
	}
	var prepare, activate probeCommandRow
	if err := tx.NewSelect().Model(&prepare).Where("command_id = ?", row.PrepareCommandID).Scan(ctx); err != nil {
		return err
	}
	if err := s.verifyCertificatePreparation(ctx, row.metadata(), *prepare.protected()); err != nil {
		return err
	}
	if err := tx.NewSelect().Model(&activate).Where("command_id = ?", row.ActivateCommandID).Scan(ctx); err != nil {
		return err
	}
	if activate.HubID != row.HubID || activate.ProbeID != row.ProbeID || activate.StreamID != row.StreamID || activate.Kind != "certificate.activate" || !activate.CreatedAt.Equal(row.CreatedAt) || !activate.ExpiresAt.Equal(row.OverlapExpiresAt) || activate.SourceAlertID != nil || activate.AssignmentGeneration != nil {
		return ports.ErrConflict
	}
	if row.PreparedAt == nil {
		if (activate.Status != "blocked" && activate.Status != "canceled") || activate.Attempts != 0 || activate.RemoteConfirmed || len(activate.ProtectedPayload) != 0 || activate.PayloadSHA256 != "" {
			return ports.ErrConflict
		}
		return nil
	}
	if !domain.ValidKeyHash(row.Fingerprint) || row.CertificateNotAfter == nil {
		return ports.ErrConflict
	}
	plain, err := s.protector.OpenCommand(ctx, activate.metadata(), activate.ProtectedPayload)
	if err != nil {
		return err
	}
	defer clear(plain)
	c, err := s.certificateCodec.DecodeCertificateCommand(ctx, plain)
	if err != nil || !certificateMatchesMetadata(c, activate.metadata()) || c.RotationID != row.RotationID || c.CertificateVersion != row.CertificateVersion || c.ExpectedFingerprint != row.Fingerprint {
		return domain.ErrValidation
	}
	return nil
}

func (s *ProbeCommandStore) completeCertificateRotation(ctx context.Context, tx bun.Tx, command probeCommandRow, result domain.ProbeCommandOutcome, now time.Time) error {
	column := "prepare_command_id"
	if command.Kind == "certificate.activate" {
		column = "activate_command_id"
	}
	var row probeCertificateRotationRow
	if err := tx.NewSelect().Model(&row).Where(column+" = ? AND hub_id = ? AND probe_id = ? AND stream_id = ?", command.CommandID, command.HubID, command.ProbeID, command.StreamID).Scan(ctx); err != nil {
		return err
	}
	if err := s.storedCertificateCommands(ctx, tx, row); err != nil {
		return err
	}
	applied := result.Status == "applied" || result.Status == "already_applied"
	if result.CredentialVersion != 0 || result.Status == "already_resolved" {
		return domain.ErrValidation
	}
	if applied && (result.AppliedAt == nil || result.AppliedAt.Before(row.CreatedAt.Add(-30*time.Second)) || !result.AppliedAt.Before(row.OverlapExpiresAt)) {
		return domain.ErrValidation
	}
	if command.Kind == "certificate.prepare" {
		if row.State != "preparing" {
			return ports.ErrConflict
		}
		state, code, activationState := "failed", result.Code, "canceled"
		var next, prepared, expiry *time.Time
		var protected []byte
		fingerprint, digest := "", ""
		if applied {
			if result.CertificateVersion != row.CertificateVersion || !domain.ValidKeyHash(result.CertificateFingerprint) || result.CertificateFingerprint == row.PreviousFingerprint || result.CertificateNotAfter == nil || !result.CertificateNotAfter.Equal(result.AppliedAt.Truncate(time.Second).Add(time.Duration(row.ValidForDays)*24*time.Hour)) {
				return domain.ErrValidation
			}
			state, code, activationState, next, prepared, expiry, fingerprint = "activating", "", "pending", &now, result.AppliedAt, result.CertificateNotAfter, result.CertificateFingerprint
			c := domain.ProbeCertificateCommand{CommandID: row.ActivateCommandID, ProbeID: row.ProbeID, Kind: "certificate.activate", CreatedAt: row.CreatedAt.UTC(), ExpiresAt: row.OverlapExpiresAt.UTC(), RotationID: row.RotationID, CertificateVersion: row.CertificateVersion, ExpectedFingerprint: fingerprint}
			plain, err := s.certificateCodec.EncodeCertificateCommand(ctx, c)
			if err != nil {
				return err
			}
			defer clear(plain)
			sum := sha256.Sum256(plain)
			digest = hex.EncodeToString(sum[:])
			m := domain.ProbeCommandMetadata{CommandID: c.CommandID, HubID: row.HubID, ProbeID: row.ProbeID, StreamID: row.StreamID, Kind: c.Kind, CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt, PayloadSHA256: digest}
			protected, err = s.protector.SealCommand(ctx, m, plain)
			if err != nil {
				return err
			}
		} else {
			if result.CertificateVersion != 0 || result.CertificateFingerprint != "" || result.CertificateNotAfter != nil {
				return domain.ErrValidation
			}
			if code == "" {
				code = result.Status
			}
		}
		var hash, pin *string
		if digest != "" {
			hash = &digest
		}
		if fingerprint != "" {
			pin = &fingerprint
		}
		changed, err := tx.NewUpdate().Model((*probeCommandRow)(nil)).Set("status = ?", activationState).Set("next_attempt_at = ?", next).Set("protected_payload = ?", protected).Set("payload_sha256 = ?", hash).Set("updated_at = ?", now).Where("command_id = ? AND status = ? AND attempts = 0 AND remote_confirmed = ? AND protected_payload IS NULL", row.ActivateCommandID, "blocked", false).Exec(ctx)
		if err != nil {
			return err
		}
		n, err := changed.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ports.ErrConflict
		}
		_, err = tx.NewUpdate().Model(&row).Set("state = ?", state).Set("prepared_at = ?", prepared).Set("certificate_not_after = ?", expiry).Set("fingerprint = ?", pin).Set("failure_code = ?", code).Set("updated_at = ?", now).WherePK().Exec(ctx)
		return err
	}
	if command.Kind != "certificate.activate" || row.State != "activating" || row.PreparedAt == nil || result.CertificateVersion != 0 || result.CertificateFingerprint != "" || result.CertificateNotAfter != nil {
		return domain.ErrValidation
	}
	state, code := "failed", result.Code
	var activated *time.Time
	if applied {
		if result.AppliedAt.Before(*row.PreparedAt) {
			return domain.ErrValidation
		}
		var current probeConnectionRow
		if err := tx.NewSelect().Model(&current).Where("probe_id = ?", row.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if current.connection().ProbeCredentialMetadata != row.metadata().Current || current.State != "active" || current.CertificateVersion != row.PreviousVersion || current.CertificateHighWater != row.CertificateVersion {
			return ports.ErrConflict
		}
		token, err := s.credentials.OpenCredential(ctx, current.connection().ProbeCredentialMetadata, current.ProtectedCredential)
		if err != nil {
			return err
		}
		promoted := current.connection().ProbeCredentialMetadata
		promoted.Fingerprint = row.Fingerprint
		cipher, err := s.credentials.SealCredential(ctx, promoted, token)
		if err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(&current).Set("fingerprint = ?", row.Fingerprint).Set("certificate_version = ?", row.CertificateVersion).Set("certificate_not_after = ?", row.CertificateNotAfter).Set("protected_credential = ?", cipher).WherePK().Exec(ctx); err != nil {
			return err
		}
		state, code, activated = "active", "", result.AppliedAt
	} else if code == "" {
		code = result.Status
	}
	_, err := tx.NewUpdate().Model(&row).Set("state = ?", state).Set("activated_at = ?", activated).Set("failure_code = ?", code).Set("updated_at = ?", now).WherePK().Exec(ctx)
	return err
}

// SelectCertificateConnection fences separate candidate TLS trust. Observed
// retirement commits even when no fallback may be returned after clock rollback.
func (s *ProbeCommandStore) SelectCertificateConnection(ctx context.Context, session domain.ProbeReplaySession) (domain.ProbeCertificateSelection, error) {
	if !validCommandSession(session) {
		return domain.ProbeCertificateSelection{}, domain.ErrValidation
	}
	var out domain.ProbeCertificateSelection
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = domain.ProbeCertificateSelection{}
		a, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		current, row, err := s.certificateConnectionState(ctx, tx, session, a.now)
		if err != nil {
			return err
		}
		out.Current = current.connection().ProbeCredentialMetadata
		if row != nil {
			out.CandidateFingerprint = row.Fingerprint
			out.RotationID = row.RotationID
			out.FallbackUntil = row.OverlapExpiresAt.UTC()
			out.AllowCurrentFallback = !row.OverlapClosed && a.now.Before(row.OverlapExpiresAt) && !a.now.Before(*row.PreparedAt)
		}
		return commandLeaseCovers(ctx, tx, a.lease, 0)
	})
	if err != nil {
		return domain.ProbeCertificateSelection{}, probeCommandError(ctx, err)
	}
	return out, nil
}

func (s *ProbeCommandStore) certificateConnectionState(ctx context.Context, tx bun.Tx, session domain.ProbeReplaySession, now time.Time) (probeConnectionRow, *probeCertificateRotationRow, error) {
	var current probeConnectionRow
	if err := tx.NewSelect().Model(&current).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
		return current, nil, err
	}
	if _, err := tx.NewUpdate().Model((*probeCertificateRotationRow)(nil)).Set("overlap_closed = ?", true).Where("probe_id = ? AND overlap_closed = ? AND overlap_expires_at <= ?", session.ProbeID, false, now).Exec(ctx); err != nil {
		return current, nil, err
	}
	var rows []probeCertificateRotationRow
	if err := tx.NewSelect().Model(&rows).Where("probe_id = ? AND state = ?", session.ProbeID, "activating").OrderExpr("certificate_version DESC").Limit(2).Scan(ctx); err != nil {
		return current, nil, err
	}
	if len(rows) > 1 {
		return current, nil, ports.ErrConflict
	}
	if len(rows) == 0 {
		return current, nil, nil
	}
	r := rows[0]
	if current.connection().ProbeCredentialMetadata != r.metadata().Current || current.State != "active" || current.CertificateVersion != r.PreviousVersion || current.CertificateHighWater != r.CertificateVersion || r.PreparedAt == nil {
		return current, nil, ports.ErrConflict
	}
	if err := s.storedCertificateCommands(ctx, tx, r); err != nil {
		return current, nil, err
	}
	return current, &r, nil
}

// ConfirmCertificateConnection validates the selected pin without promoting it.
// Rejection still preserves an observed overlap retirement in the transaction.
func (s *ProbeCommandStore) ConfirmCertificateConnection(ctx context.Context, session domain.ProbeReplaySession, metadata domain.ProbeCredentialMetadata, pin string) error {
	if !validCommandSession(session) || !domain.ValidProbeCredentialMetadata(metadata) || metadata.HubID != session.HubID || metadata.ProbeID != session.ProbeID || metadata.StreamID != session.StreamID || !domain.ValidKeyHash(pin) {
		return domain.ErrValidation
	}
	accepted := false
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		accepted = false
		a, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		current, row, err := s.certificateConnectionState(ctx, tx, session, a.now)
		if err != nil {
			return err
		}
		credentialMatches, err := s.certificateCredentialMatches(ctx, tx, current, metadata)
		if err != nil {
			return err
		}
		if credentialMatches {
			accepted = pin == current.Fingerprint
			if row != nil {
				accepted = pin == row.Fingerprint || (accepted && !row.OverlapClosed && a.now.Before(row.OverlapExpiresAt) && !a.now.Before(*row.PreparedAt))
			}
		}
		return commandLeaseCovers(ctx, tx, a.lease, 0)
	})
	if err != nil {
		return probeCommandError(ctx, err)
	}
	if !accepted {
		return ports.ErrConflict
	}
	return nil
}

func (s *ProbeCommandStore) certificateCredentialMatches(ctx context.Context, tx bun.Tx, current probeConnectionRow, metadata domain.ProbeCredentialMetadata) (bool, error) {
	if current.connection().ProbeCredentialMetadata == metadata {
		return true, nil
	}
	// Credential rotation may be authenticating its candidate while certificate
	// trust stays unchanged. Verify that actual retained candidate under this fence.
	var rows []probeCredentialRotationRow
	if err := tx.NewSelect().Model(&rows).Where("probe_id = ? AND credential_version = ? AND state = ?", current.ProbeID, metadata.CredentialVersion, "activating").Limit(2).Scan(ctx); err != nil {
		return false, err
	}
	if len(rows) != 1 {
		return false, nil
	}
	r := rows[0]
	expected := metadata
	expected.CredentialVersion = r.PreviousVersion
	if r.metadata().Candidate != metadata || current.connection().ProbeCredentialMetadata != expected || current.State != "active" || current.CredentialHighWater != r.CredentialVersion {
		return false, nil
	}
	if err := s.storedRotationCommands(ctx, tx, r); err != nil {
		return false, err
	}
	return true, nil
}
