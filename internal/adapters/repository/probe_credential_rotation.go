package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxProbeCredentialRotations = 1024

var _ ports.ProbeCredentialRotationRepository = (*ProbeCommandStore)(nil)

type probeCredentialRotationRow struct {
	bun.BaseModel       `bun:"table:probe_credential_rotations"`
	RotationID          string `bun:"rotation_id,pk"`
	HubID               string
	ProbeID             string
	StreamID            string
	EnrollmentID        string
	CredentialVersion   int64
	PreviousVersion     int64
	Endpoint            string
	Fingerprint         string
	ProtectedCredential []byte
	PrepareCommandID    string
	ActivateCommandID   string
	CreatedAt           time.Time
	OverlapExpiresAt    time.Time
	State               string
	PreparedAt          *time.Time
	ActivatedAt         *time.Time
	FailureCode         string
	UpdatedAt           time.Time
	RetainUntil         time.Time
}

func (r probeCredentialRotationRow) metadata() domain.ProbeCredentialRotation {
	return domain.ProbeCredentialRotation{RotationID: r.RotationID, Candidate: domain.ProbeCredentialMetadata{HubID: r.HubID, ProbeID: r.ProbeID, StreamID: r.StreamID, EnrollmentID: r.EnrollmentID, CredentialVersion: r.CredentialVersion, Endpoint: r.Endpoint, Fingerprint: r.Fingerprint}, PreviousVersion: r.PreviousVersion, PrepareCommandID: r.PrepareCommandID, ActivateCommandID: r.ActivateCommandID, CreatedAt: r.CreatedAt.UTC(), OverlapExpiresAt: r.OverlapExpiresAt.UTC(), State: r.State, PreparedAt: utcTimePtr(r.PreparedAt), ActivatedAt: utcTimePtr(r.ActivatedAt), FailureCode: r.FailureCode, UpdatedAt: r.UpdatedAt.UTC()}
}

func (r probeCredentialRotationRow) candidate() *domain.ProbeConnection {
	return &domain.ProbeConnection{ProbeCredentialMetadata: r.metadata().Candidate, ProtectedCredential: r.ProtectedCredential, State: "prepared", PreparedAt: r.CreatedAt.UTC()}
}

// CreateCredentialRotation persists the candidate and both exact requests under
// current installation authority. Activation starts blocked and cannot dispatch
// until successful source preparation is durably confirmed.
func (s *ProbeCommandStore) CreateCredentialRotation(ctx context.Context, c domain.ProtectedProbeCredentialRotation) (*domain.ProbeCredentialRotation, error) {
	if s == nil || s.db == nil || s.protector == nil || s.credentials == nil || s.credentialCodec == nil || c.State != "preparing" || c.PreparedAt != nil || c.ActivatedAt != nil || c.FailureCode != "" {
		return nil, domain.ErrValidation
	}
	if err := s.verifyRotationPayloads(ctx, c.ProbeCredentialRotation, c.ProtectedCredential, c.PrepareCommand, c.ActivateCommand); err != nil {
		return nil, err
	}
	var out *domain.ProbeCredentialRotation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		m := c.Candidate
		if err := s.lockCommandTarget(ctx, tx, m.HubID, m.ProbeID, m.StreamID); err != nil {
			return err
		}
		var existing probeCredentialRotationRow
		err := tx.NewSelect().Model(&existing).Where("rotation_id = ?", c.RotationID).Scan(ctx)
		if err == nil {
			if !sameRotationIssuance(existing.metadata(), c.ProbeCredentialRotation) {
				return ports.ErrConflict
			}
			for _, request := range []domain.ProtectedProbeCommand{c.PrepareCommand, c.ActivateCommand} {
				var saved probeCommandRow
				if err := tx.NewSelect().Model(&saved).Where("command_id = ?", request.CommandID).Scan(ctx); err != nil {
					return err
				}
				if !sameCommandMetadata(saved.metadata(), request.ProbeCommandMetadata) {
					return ports.ErrConflict
				}
			}
			result := existing.metadata()
			out = &result
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
		expected := m
		expected.CredentialVersion = c.PreviousVersion
		if current.connection().ProbeCredentialMetadata != expected || current.State != "active" || m.CredentialVersion <= current.CredentialHighWater {
			return ports.ErrConflict
		}
		currentToken, err := s.credentials.OpenCredential(ctx, expected, current.ProtectedCredential)
		if err != nil {
			return err
		}
		candidateToken, err := s.credentials.OpenCredential(ctx, m, c.ProtectedCredential)
		if err != nil {
			return err
		}
		if sha256.Sum256([]byte(currentToken)) == sha256.Sum256([]byte(candidateToken)) {
			return ports.ErrConflict
		}
		busy, err := tx.NewSelect().Model((*probeCredentialRotationRow)(nil)).Where("probe_id = ? AND (state IN (?, ?) OR overlap_expires_at > ?)", m.ProbeID, "preparing", "activating", now).Exists(ctx)
		if err != nil {
			return err
		}
		if busy {
			return ports.ErrConflict
		}
		if _, err := tx.NewDelete().Model((*probeCredentialRotationRow)(nil)).Where("probe_id = ? AND state IN (?, ?) AND retain_until < ?", m.ProbeID, "active", "failed", now).Exec(ctx); err != nil {
			return err
		}
		count, err := tx.NewSelect().Model((*probeCredentialRotationRow)(nil)).Where("probe_id = ?", m.ProbeID).Count(ctx)
		if err != nil {
			return err
		}
		if count >= maxProbeCredentialRotations {
			return ports.ErrConflict
		}
		if err := reserveCommandCapacity(ctx, tx, m.ProbeID, now, 2, len(c.PrepareCommand.ProtectedPayload)+len(c.ActivateCommand.ProtectedPayload)); err != nil {
			return err
		}
		for index, request := range []domain.ProtectedProbeCommand{c.PrepareCommand, c.ActivateCommand} {
			row := newPendingCommandRow(request, now)
			if index == 1 {
				row.Status = "blocked"
				row.NextAttemptAt = nil
			}
			if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
				return err
			}
		}
		row := probeCredentialRotationRow{RotationID: c.RotationID, HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID, EnrollmentID: m.EnrollmentID, CredentialVersion: m.CredentialVersion, PreviousVersion: c.PreviousVersion, Endpoint: m.Endpoint, Fingerprint: m.Fingerprint, ProtectedCredential: c.ProtectedCredential, PrepareCommandID: c.PrepareCommandID, ActivateCommandID: c.ActivateCommandID, CreatedAt: c.CreatedAt.UTC(), OverlapExpiresAt: c.OverlapExpiresAt.UTC(), State: "preparing", UpdatedAt: now, RetainUntil: c.OverlapExpiresAt.Add(probeCommandRetention).UTC()}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(&current).Set("credential_high_water = ?", m.CredentialVersion).WherePK().Exec(ctx); err != nil {
			return err
		}
		result := row.metadata()
		out = &result
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	return out, nil
}

// GetCredentialRotation returns only nonsecret operation metadata to its owner.
func (s *ProbeCommandStore) GetCredentialRotation(ctx context.Context, hubID, probeID, rotationID string) (*domain.ProbeCredentialRotation, error) {
	if !domain.ValidHubID(hubID) || !domain.ValidHubID(probeID) || !domain.ValidHubID(rotationID) {
		return nil, domain.ErrValidation
	}
	var row probeCredentialRotationRow
	if err := s.db.NewSelect().Model(&row).Where("hub_id = ? AND probe_id = ? AND rotation_id = ?", hubID, probeID, rotationID).Scan(ctx); err != nil {
		return nil, probeCommandError(ctx, err)
	}
	out := row.metadata()
	return &out, nil
}

func (s *ProbeCommandStore) verifyRotationPayloads(ctx context.Context, r domain.ProbeCredentialRotation, cipher []byte, prepare, activate domain.ProtectedProbeCommand) error {
	m := r.Candidate
	if !domain.ValidProbeCredentialMetadata(m) || !domain.ValidHubID(r.RotationID) || !domain.ValidHubID(r.PrepareCommandID) || !domain.ValidHubID(r.ActivateCommandID) || r.PrepareCommandID == r.ActivateCommandID || r.RotationID == r.PrepareCommandID || r.RotationID == r.ActivateCommandID || r.PreviousVersion <= 0 || m.CredentialVersion <= r.PreviousVersion || r.CreatedAt.IsZero() || r.CreatedAt.Nanosecond()%1000 != 0 || !r.OverlapExpiresAt.Equal(r.CreatedAt.Add(domain.ProbeCredentialOverlap)) {
		return domain.ErrValidation
	}
	token, err := s.credentials.OpenCredential(ctx, m, cipher)
	if err != nil {
		return err
	}
	for index, request := range []domain.ProtectedProbeCommand{prepare, activate} {
		kind, id := "credential.prepare", r.PrepareCommandID
		if index == 1 {
			kind, id = "credential.activate", r.ActivateCommandID
		}
		meta := request.ProbeCommandMetadata
		if !domain.ValidProbeCommandMetadata(meta) || meta.CommandID != id || meta.Kind != kind || meta.HubID != m.HubID || meta.ProbeID != m.ProbeID || meta.StreamID != m.StreamID || !meta.CreatedAt.Equal(r.CreatedAt) || !meta.ExpiresAt.Equal(r.OverlapExpiresAt) {
			return domain.ErrValidation
		}
		plain, err := s.protector.OpenCommand(ctx, meta, request.ProtectedPayload)
		if err != nil {
			return err
		}
		command, err := s.credentialCodec.DecodeCredentialCommand(ctx, plain)
		clear(plain)
		if err != nil || !credentialMatchesMetadata(command, meta) || command.RotationID != r.RotationID || command.CredentialVersion != m.CredentialVersion {
			return domain.ErrValidation
		}
		if index == 0 && (command.TokenHash != sha256.Sum256([]byte(token)) || !command.OverlapExpiresAt.Equal(r.OverlapExpiresAt)) {
			return domain.ErrValidation
		}
	}
	return nil
}

func credentialMatchesMetadata(c domain.ProbeCredentialCommand, m domain.ProbeCommandMetadata) bool {
	return m.Kind == c.Kind && m.CommandID == c.CommandID && m.ProbeID == c.ProbeID && m.SourceAlertID == nil && m.AssignmentGeneration == nil && m.CreatedAt.Equal(c.CreatedAt) && m.ExpiresAt.Equal(c.ExpiresAt) && m.PayloadSHA256 == hex.EncodeToString(c.PayloadHash[:])
}

func sameRotationIssuance(a, b domain.ProbeCredentialRotation) bool {
	return a.RotationID == b.RotationID && a.Candidate == b.Candidate && a.PreviousVersion == b.PreviousVersion && a.PrepareCommandID == b.PrepareCommandID && a.ActivateCommandID == b.ActivateCommandID && a.CreatedAt.Equal(b.CreatedAt) && a.OverlapExpiresAt.Equal(b.OverlapExpiresAt)
}

func newPendingCommandRow(c domain.ProtectedProbeCommand, now time.Time) probeCommandRow {
	retain := c.ExpiresAt.UTC().Add(probeCommandRetention)
	return probeCommandRow{CommandID: c.CommandID, HubID: c.HubID, ProbeID: c.ProbeID, StreamID: c.StreamID, Kind: c.Kind, SourceAlertID: c.SourceAlertID, AssignmentGeneration: c.AssignmentGeneration, CreatedAt: c.CreatedAt.UTC(), ExpiresAt: c.ExpiresAt.UTC(), PayloadSHA256: c.PayloadSHA256, ProtectedPayload: c.ProtectedPayload, Status: "pending", NextAttemptAt: &now, RetainUntil: &retain, UpdatedAt: now}
}

func reserveCommandCapacity(ctx context.Context, tx bun.Tx, probeID string, now time.Time, additionalCount, additionalBytes int) error {
	// A confirmed prepare is still a dependency of unresolved activation. Keep
	// both immutable bodies for every retained rotation, regardless of the
	// individual receipt's age. Terminal rotation pruning releases them first.
	if _, err := tx.NewDelete().Model((*probeCommandRow)(nil)).Where("probe_id = ? AND retain_until < ? AND (remote_confirmed = ? OR (status = ? AND attempts = 0))", probeID, now, true, "canceled").
		Where("command_id NOT IN (SELECT prepare_command_id FROM probe_credential_rotations WHERE probe_id = ? UNION ALL SELECT activate_command_id FROM probe_credential_rotations WHERE probe_id = ?)", probeID, probeID).Exec(ctx); err != nil {
		return err
	}
	count, err := tx.NewSelect().Model((*probeCommandRow)(nil)).Where("probe_id = ?", probeID).Count(ctx)
	if err != nil {
		return err
	}
	pending, err := tx.NewSelect().Model((*probeCommandRow)(nil)).Where("probe_id = ? AND remote_confirmed = ? AND status IN (?, ?)", probeID, false, "pending", "blocked").Count(ctx)
	if err != nil {
		return err
	}
	length := "length(protected_payload)"
	if tx.Dialect().Name() == dialect.MySQL {
		length = "OCTET_LENGTH(protected_payload)"
	}
	var size int64
	if err := tx.NewRaw("SELECT COALESCE(SUM("+length+"),0) FROM probe_commands WHERE probe_id = ?", probeID).Scan(ctx, &size); err != nil {
		return err
	}
	if count > maxProbeCommands-additionalCount || pending > maxPendingProbeCommands-additionalCount || size > maxProbeCommandStorageBytes-int64(additionalBytes) {
		return ports.ErrConflict
	}
	return nil
}
