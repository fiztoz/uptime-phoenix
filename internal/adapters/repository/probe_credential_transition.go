package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func (s *ProbeCommandStore) storedRotationCommands(ctx context.Context, tx bun.Tx, row probeCredentialRotationRow) error {
	var prepare, activate probeCommandRow
	if err := tx.NewSelect().Model(&prepare).Where("command_id = ?", row.PrepareCommandID).Scan(ctx); err != nil {
		return err
	}
	if err := tx.NewSelect().Model(&activate).Where("command_id = ?", row.ActivateCommandID).Scan(ctx); err != nil {
		return err
	}
	return s.verifyRotationPayloads(ctx, row.metadata(), row.ProtectedCredential, *prepare.protected(), *activate.protected())
}

// SelectCredentialConnection reads current and optional candidate under the same
// DB lease used by the upcoming socket. Nothing here confirms source activation.
func (s *ProbeCommandStore) SelectCredentialConnection(ctx context.Context, session domain.ProbeReplaySession) (domain.ProbeCredentialSelection, error) {
	if !validCommandSession(session) || s.credentials == nil || s.credentialCodec == nil {
		return domain.ProbeCredentialSelection{}, domain.ErrValidation
	}
	var out domain.ProbeCredentialSelection
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = domain.ProbeCredentialSelection{}
		authority, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		var current probeConnectionRow
		if err := tx.NewSelect().Model(&current).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		out.Current = *current.connection()
		var pending []probeCredentialRotationRow
		if err := tx.NewSelect().Model(&pending).Where("probe_id = ? AND state = ?", session.ProbeID, "activating").OrderExpr("credential_version DESC").Limit(2).Scan(ctx); err != nil {
			return err
		}
		if len(pending) > 1 {
			return ports.ErrConflict
		}
		if len(pending) == 1 {
			rotation := pending[0]
			expected := rotation.metadata().Candidate
			expected.CredentialVersion = rotation.PreviousVersion
			if current.connection().ProbeCredentialMetadata != expected || current.State != "active" || current.CredentialHighWater != rotation.CredentialVersion {
				return ports.ErrConflict
			}
			if err := s.storedRotationCommands(ctx, tx, rotation); err != nil {
				return err
			}
			out.Candidate = rotation.candidate()
			out.RotationID = rotation.RotationID
		}
		return commandLeaseCovers(ctx, tx, authority.lease, 0)
	})
	if err != nil {
		return domain.ProbeCredentialSelection{}, probeCommandError(ctx, err)
	}
	return out, nil
}

// ConfirmCredentialConnection confirms authenticated enrollment or a selected
// candidate. Candidate health never promotes the saved current credential.
func (s *ProbeCommandStore) ConfirmCredentialConnection(ctx context.Context, session domain.ProbeReplaySession, metadata domain.ProbeCredentialMetadata) error {
	if !validCommandSession(session) || !domain.ValidProbeCredentialMetadata(metadata) || metadata.HubID != session.HubID || metadata.ProbeID != session.ProbeID || metadata.StreamID != session.StreamID {
		return domain.ErrValidation
	}
	return probeCommandError(ctx, runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		authority, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		var current probeConnectionRow
		if err := tx.NewSelect().Model(&current).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if current.connection().ProbeCredentialMetadata == metadata {
			if current.State == "prepared" {
				if _, err := tx.NewUpdate().Model(&current).Set("state = ?", "active").Set("activated_at = ?", authority.now).WherePK().Exec(ctx); err != nil {
					return err
				}
			} else if current.State != "active" {
				return ports.ErrConflict
			}
		} else {
			var rotation probeCredentialRotationRow
			if err := tx.NewSelect().Model(&rotation).Where("hub_id = ? AND probe_id = ? AND credential_version = ? AND state = ?", session.HubID, session.ProbeID, metadata.CredentialVersion, "activating").Scan(ctx); err != nil {
				return err
			}
			expected := metadata
			expected.CredentialVersion = rotation.PreviousVersion
			if rotation.metadata().Candidate != metadata || current.connection().ProbeCredentialMetadata != expected || current.State != "active" || current.CredentialHighWater != rotation.CredentialVersion {
				return ports.ErrConflict
			}
			if err := s.storedRotationCommands(ctx, tx, rotation); err != nil {
				return err
			}
		}
		return commandLeaseCovers(ctx, tx, authority.lease, 0)
	}))
}

func (s *ProbeCommandStore) completeCredentialRotation(ctx context.Context, tx bun.Tx, command probeCommandRow, result domain.ProbeCommandOutcome, now time.Time) error {
	if s.credentials == nil || s.credentialCodec == nil {
		return domain.ErrValidation
	}
	var row probeCredentialRotationRow
	column := "prepare_command_id"
	if command.Kind == "credential.activate" {
		column = "activate_command_id"
	}
	err := tx.NewSelect().Model(&row).Where(column+" = ? AND hub_id = ? AND probe_id = ? AND stream_id = ?", command.CommandID, command.HubID, command.ProbeID, command.StreamID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrConflict
	}
	if err != nil {
		return err
	}
	if err := s.storedRotationCommands(ctx, tx, row); err != nil {
		return err
	}
	applied := result.Status == "applied" || result.Status == "already_applied"
	if result.CertificateVersion != 0 || result.CertificateFingerprint != "" || result.CertificateNotAfter != nil || result.Status == "already_resolved" || result.CredentialVersion != 0 && !(command.Kind == "credential.prepare" && applied) {
		return domain.ErrValidation
	}
	if applied && (result.AppliedAt == nil || result.AppliedAt.Before(row.CreatedAt.Add(-30*time.Second)) || !result.AppliedAt.Before(row.OverlapExpiresAt)) {
		return domain.ErrValidation
	}
	if command.Kind == "credential.prepare" {
		if row.State != "preparing" {
			return ports.ErrConflict
		}
		state, code, next := "failed", result.Code, (*time.Time)(nil)
		activationState := "canceled"
		var preparedAt *time.Time
		if applied {
			if result.CredentialVersion != row.CredentialVersion {
				return domain.ErrValidation
			}
			state, code, next, activationState, preparedAt = "activating", "", &now, "pending", result.AppliedAt
		} else if code == "" {
			code = result.Status
		}
		changed, err := tx.NewUpdate().Model((*probeCommandRow)(nil)).Set("status = ?", activationState).Set("next_attempt_at = ?", next).Set("updated_at = ?", now).Where("command_id = ? AND status = ? AND attempts = 0 AND remote_confirmed = ?", row.ActivateCommandID, "blocked", false).Exec(ctx)
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
		_, err = tx.NewUpdate().Model(&row).Set("state = ?", state).Set("prepared_at = ?", preparedAt).Set("failure_code = ?", code).Set("updated_at = ?", now).WherePK().Exec(ctx)
		return err
	}
	if command.Kind != "credential.activate" || row.State != "activating" || row.PreparedAt == nil {
		return ports.ErrConflict
	}
	state, code := "failed", result.Code
	var activatedAt *time.Time
	if applied {
		var current probeConnectionRow
		if err := tx.NewSelect().Model(&current).Where("probe_id = ?", row.ProbeID).Scan(ctx); err != nil {
			return err
		}
		expected := row.metadata().Candidate
		expected.CredentialVersion = row.PreviousVersion
		if current.connection().ProbeCredentialMetadata != expected || current.State != "active" || current.CredentialHighWater != row.CredentialVersion {
			return ports.ErrConflict
		}
		if _, err := tx.NewUpdate().Model(&current).Set("credential_version = ?", row.CredentialVersion).Set("protected_credential = ?", row.ProtectedCredential).Set("prepared_at = ?", row.CreatedAt).Set("activated_at = ?", result.AppliedAt).WherePK().Exec(ctx); err != nil {
			return err
		}
		state, code, activatedAt = "active", "", result.AppliedAt
	} else if code == "" {
		code = result.Status
	}
	_, err = tx.NewUpdate().Model(&row).Set("state = ?", state).Set("activated_at = ?", activatedAt).Set("failure_code = ?", code).Set("updated_at = ?", now).WherePK().Exec(ctx)
	return err
}
