package repository

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxProbeCommands = 16384
const maxPendingProbeCommands = 1024
const maxProbeCommandStorageBytes = 64 << 20
const probeCommandRetention = 365 * 24 * time.Hour

// ProbeCommandStore persists exact protected requests and source confirmation on
// either hub engine. It performs no socket I/O and never logs command plaintext.
type ProbeCommandStore struct {
	db              *bun.DB
	protector       ports.ProbeCommandProtector
	codec           ports.ProbeAcknowledgementCodec
	credentials     ports.ProbeCredentialProtector
	credentialCodec ports.ProbeCredentialCommandCodec
}

var _ ports.ProbeCommandRepository = (*ProbeCommandStore)(nil)

// NewProbeCommandStore shares the verified installation key and closed wire codec.
func NewProbeCommandStore(db *bun.DB, protector ports.ProbeCommandProtector, codec ports.ProbeAcknowledgementCodec, credentials ports.ProbeCredentialProtector, credentialCodec ports.ProbeCredentialCommandCodec) *ProbeCommandStore {
	return &ProbeCommandStore{db: db, protector: protector, codec: codec, credentials: credentials, credentialCodec: credentialCodec}
}

type probeCommandRow struct {
	bun.BaseModel           `bun:"table:probe_commands"`
	CommandID               string `bun:"command_id,pk"`
	HubID                   string `bun:"hub_id,nullzero"`
	ProbeID                 string
	StreamID                string `bun:"stream_id,nullzero"`
	Kind                    string
	SourceAlertID           *string
	AssignmentGeneration    *int64
	CreatedAt               time.Time
	ExpiresAt               time.Time
	PayloadSHA256           string `bun:"payload_sha256,nullzero"`
	ProtectedPayload        []byte
	Status                  string
	RemoteConfirmed         bool
	Attempts                int64
	LastAttemptAt           *time.Time
	NextAttemptAt           *time.Time
	ResultAppliedAt         *time.Time
	ResultCode              string
	ResultMessage           string
	ResultCredentialVersion int64 `bun:"result_credential_version,nullzero"`
	RetainUntil             *time.Time
	UpdatedAt               time.Time
}

func (r probeCommandRow) metadata() domain.ProbeCommandMetadata {
	return domain.ProbeCommandMetadata{CommandID: r.CommandID, HubID: r.HubID, ProbeID: r.ProbeID, StreamID: r.StreamID, Kind: r.Kind, SourceAlertID: r.SourceAlertID, AssignmentGeneration: r.AssignmentGeneration, CreatedAt: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), PayloadSHA256: r.PayloadSHA256}
}

func (r probeCommandRow) command() *domain.ProbeCommand {
	c := &domain.ProbeCommand{ProbeCommandMetadata: r.metadata(), Status: r.Status, RemoteConfirmed: r.RemoteConfirmed, Attempts: r.Attempts, LastAttemptAt: utcTimePtr(r.LastAttemptAt), UpdatedAt: r.UpdatedAt.UTC()}
	if r.NextAttemptAt != nil {
		c.NextAttemptAt = r.NextAttemptAt.UTC()
	}
	if r.RemoteConfirmed {
		c.Outcome = &domain.ProbeCommandOutcome{CommandID: r.CommandID, Status: r.Status, AppliedAt: utcTimePtr(r.ResultAppliedAt), Code: r.ResultCode, Message: r.ResultMessage, CredentialVersion: r.ResultCredentialVersion}
	}
	return c
}

func (r probeCommandRow) protected() *domain.ProtectedProbeCommand {
	return &domain.ProtectedProbeCommand{ProbeCommandMetadata: r.metadata(), ProtectedPayload: r.ProtectedPayload}
}

// CreateCommand authorizes an ACK of a known source incident and persists the
// immutable request before dispatch. Duplicate issuance never replaces ciphertext.
func (s *ProbeCommandStore) CreateCommand(ctx context.Context, c domain.ProtectedProbeCommand) (*domain.ProbeCommand, error) {
	if s == nil || s.db == nil || s.protector == nil || s.codec == nil || !domain.ValidProbeCommandMetadata(c.ProbeCommandMetadata) || c.Kind != "alert.ack" {
		return nil, domain.ErrValidation
	}
	plain, err := s.protector.OpenCommand(ctx, c.ProbeCommandMetadata, c.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	ack, err := s.codec.DecodeAcknowledgement(ctx, plain)
	clear(plain)
	if err != nil || !acknowledgementMatchesMetadata(ack, c.ProbeCommandMetadata) {
		return nil, domain.ErrValidation
	}
	var out *domain.ProbeCommand
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		if err := s.lockCommandTarget(ctx, tx, c.HubID, c.ProbeID, c.StreamID); err != nil {
			return err
		}
		var existing probeCommandRow
		err := tx.NewSelect().Model(&existing).Where("command_id = ?", c.CommandID).Scan(ctx)
		if err == nil {
			if !sameCommandMetadata(existing.metadata(), c.ProbeCommandMetadata) {
				return ports.ErrConflict
			}
			out = existing.command()
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if !now.Before(c.ExpiresAt) || c.CreatedAt.After(now.Add(30*time.Second)) {
			return domain.ErrValidation
		}
		var incident probeIncidentModel
		if err := tx.NewSelect().Model(&incident).Where("source_alert_id = ? AND probe_id = ? AND assignment_generation = ? AND scope = ? AND subject_kind = ?", *c.SourceAlertID, c.ProbeID, *c.AssignmentGeneration, domain.IncidentScopeRegional, domain.IncidentSubjectAvailability).Scan(ctx); err != nil {
			return err
		}
		if err := reserveCommandCapacity(ctx, tx, c.ProbeID, now, 1, len(c.ProtectedPayload)); err != nil {
			return err
		}
		row := newPendingCommandRow(c, now)
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		out = row.command()
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	return out, nil
}

func (s *ProbeCommandStore) lockCommandTarget(ctx context.Context, tx bun.Tx, hubID, probeID, streamID string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", probeID); err != nil {
		return err
	}
	var registration probeRegistrationModel
	if err := tx.NewSelect().Model(&registration).Where("id = ?", probeID).Scan(ctx); err != nil {
		return err
	}
	if !registration.Enabled || registration.Kind != domain.ProbeKindRemote {
		return ports.ErrConflict
	}
	var installation probeInstallationModel
	if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
		return err
	}
	if installation.HubID != hubID || installation.KeyHash != s.protector.KeyHash(hubID) {
		return domain.ErrProbeKeyMismatch
	}
	var connection probeConnectionRow
	if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", probeID).Scan(ctx); err != nil {
		return err
	}
	if connection.HubID != hubID || connection.StreamID != streamID || connection.State != "active" {
		return ports.ErrConflict
	}
	var stream probeStreamModel
	if err := tx.NewSelect().Model(&stream).Where("probe_id = ? AND stream_id = ?", probeID, streamID).Scan(ctx); err != nil {
		return err
	}
	if stream.RetiredAt != nil {
		return ports.ErrConflict
	}
	return nil
}

// GetCommand exposes metadata and the nonsecret source result, never its payload.
func (s *ProbeCommandStore) GetCommand(ctx context.Context, hubID, probeID, commandID string) (*domain.ProbeCommand, error) {
	row, err := s.readCommand(ctx, hubID, probeID, commandID)
	if err != nil {
		return nil, err
	}
	return row.command(), nil
}

// GetProtectedCommand is for trusted retries/authorization, not a wire response.
func (s *ProbeCommandStore) GetProtectedCommand(ctx context.Context, hubID, probeID, commandID string) (*domain.ProtectedProbeCommand, error) {
	row, err := s.readCommand(ctx, hubID, probeID, commandID)
	if err != nil {
		return nil, err
	}
	return row.protected(), nil
}

func (s *ProbeCommandStore) readCommand(ctx context.Context, hubID, probeID, commandID string) (probeCommandRow, error) {
	if !domain.ValidHubID(hubID) || !validRemoteProbeID(probeID) || !domain.ValidHubID(commandID) {
		return probeCommandRow{}, domain.ErrValidation
	}
	var row probeCommandRow
	err := s.db.NewSelect().Model(&row).Where("hub_id = ? AND probe_id = ? AND command_id = ?", hubID, probeID, commandID).Scan(ctx)
	return row, probeCommandError(ctx, err)
}

// ClaimCommand reserves one due retry under the current connector authority.
// Expired requests are still sent to recover an already-applied source result.
func (s *ProbeCommandStore) ClaimCommand(ctx context.Context, session domain.ProbeReplaySession, budget time.Duration, capabilities domain.ProbeCommandCapabilities) (*domain.ProtectedProbeCommand, error) {
	if !validCommandSession(session) || budget <= 0 || budget > 10*time.Second {
		return nil, domain.ErrValidation
	}
	var kinds []string
	if capabilities.AlertAcknowledgement {
		kinds = append(kinds, "alert.ack")
	}
	if capabilities.CredentialRotation {
		kinds = append(kinds, "credential.prepare", "credential.activate")
	}
	if len(kinds) == 0 {
		return nil, nil
	}
	var out *domain.ProtectedProbeCommand
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		authority, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		var row probeCommandRow
		err = tx.NewSelect().Model(&row).Where("hub_id = ? AND probe_id = ? AND stream_id = ? AND kind IN (?) AND remote_confirmed = ? AND status = ? AND next_attempt_at <= ? AND protected_payload IS NOT NULL", session.HubID, session.ProbeID, session.StreamID, bun.List(kinds), false, "pending", authority.now).OrderExpr("next_attempt_at ASC, created_at ASC, command_id ASC").Limit(1).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if row.Attempts < 0 || row.Attempts == math.MaxInt64 {
			return ports.ErrConflict
		}
		delay := min(60*time.Second, time.Second*time.Duration(int64(1)<<min(row.Attempts, 6)))
		next := authority.now.Add(delay)
		if _, err := tx.NewUpdate().Model(&row).Set("attempts = ?", row.Attempts+1).Set("last_attempt_at = ?", authority.now).Set("next_attempt_at = ?", next).Set("updated_at = ?", authority.now).WherePK().Exec(ctx); err != nil {
			return err
		}
		if err := commandLeaseCovers(ctx, tx, authority.lease, budget); err != nil {
			return err
		}
		out = row.protected()
		return nil
	})
	if err != nil {
		return nil, probeCommandError(ctx, err)
	}
	return out, nil
}

// CompleteCommand records only a durable source result received on current
// session authority. Contradictory duplicate results never overwrite history.
func (s *ProbeCommandStore) CompleteCommand(ctx context.Context, session domain.ProbeReplaySession, result domain.ProbeCommandOutcome) error {
	if !validCommandSession(session) || !validCommandOutcome(result) {
		return domain.ErrValidation
	}
	if result.AppliedAt != nil {
		at := result.AppliedAt.UTC().Truncate(time.Microsecond)
		result.AppliedAt = &at
	}
	return probeCommandError(ctx, runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		authority, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		var row probeCommandRow
		if err := tx.NewSelect().Model(&row).Where("command_id = ? AND hub_id = ? AND probe_id = ? AND stream_id = ?", result.CommandID, session.HubID, session.ProbeID, session.StreamID).Scan(ctx); err != nil {
			return err
		}
		if row.Attempts < 1 || (row.Kind != "alert.ack" && row.Kind != "credential.prepare" && row.Kind != "credential.activate") {
			return ports.ErrConflict
		}
		if row.RemoteConfirmed {
			if !sameCommandOutcome(*row.command().Outcome, result) {
				return ports.ErrConflict
			}
		} else {
			if row.Status != "pending" {
				return ports.ErrConflict
			}
			if row.Kind == "alert.ack" {
				if result.CredentialVersion != 0 {
					return domain.ErrValidation
				}
			} else if err := s.completeCredentialRotation(ctx, tx, row, result, authority.now); err != nil {
				return err
			}
			var credentialVersion *int64
			if result.CredentialVersion > 0 {
				credentialVersion = &result.CredentialVersion
			}
			if _, err := tx.NewUpdate().Model(&row).Set("result_credential_version = ?", credentialVersion).Set("status = ?", result.Status).Set("remote_confirmed = ?", true).Set("result_applied_at = ?", result.AppliedAt).Set("result_code = ?", result.Code).Set("result_message = ?", result.Message).Set("updated_at = ?", authority.now).WherePK().Exec(ctx); err != nil {
				return err
			}
		}
		return commandLeaseCovers(ctx, tx, authority.lease, 0)
	}))
}

func commandLeaseCovers(ctx context.Context, tx bun.Tx, lease probeSessionModel, budget time.Duration) error {
	now, err := replayDatabaseTime(ctx, tx)
	if err != nil {
		return err
	}
	if !now.Add(budget).Before(time.Unix(lease.LeaseUntil, 0).UTC()) {
		return ports.ErrConflict
	}
	return nil
}

func acknowledgementMatchesMetadata(c domain.ProbeAlertAcknowledgement, m domain.ProbeCommandMetadata) bool {
	return m.Kind == "alert.ack" && c.CommandID == m.CommandID && c.ProbeID == m.ProbeID && m.SourceAlertID != nil && c.SourceAlertID == *m.SourceAlertID && m.AssignmentGeneration != nil && c.AssignmentGeneration == *m.AssignmentGeneration && c.CreatedAt.Equal(m.CreatedAt) && c.ExpiresAt.Equal(m.ExpiresAt) && hex.EncodeToString(c.PayloadHash[:]) == m.PayloadSHA256
}

func sameCommandMetadata(a, b domain.ProbeCommandMetadata) bool {
	return a.CommandID == b.CommandID && a.HubID == b.HubID && a.ProbeID == b.ProbeID && a.StreamID == b.StreamID && a.Kind == b.Kind && a.PayloadSHA256 == b.PayloadSHA256 && a.CreatedAt.Equal(b.CreatedAt) && a.ExpiresAt.Equal(b.ExpiresAt) && (a.SourceAlertID == nil) == (b.SourceAlertID == nil) && (a.SourceAlertID == nil || *a.SourceAlertID == *b.SourceAlertID) && (a.AssignmentGeneration == nil) == (b.AssignmentGeneration == nil) && (a.AssignmentGeneration == nil || *a.AssignmentGeneration == *b.AssignmentGeneration)
}

func validCommandSession(s domain.ProbeReplaySession) bool {
	return domain.ValidHubID(s.HubID) && validRemoteProbeID(s.ProbeID) && domain.ValidHubID(s.StreamID) && domain.ValidHubID(s.OwnerID) && s.ConnectionGeneration > 0
}

func validCommandOutcome(r domain.ProbeCommandOutcome) bool {
	// Certificate dispatch is not enabled until its separate hub journal is wired.
	if r.CredentialVersion < 0 || r.CertificateVersion != 0 || r.CertificateFingerprint != "" || r.CertificateNotAfter != nil {
		return false
	}
	if !domain.ValidHubID(r.CommandID) || len(r.Message) > 4096 || len(r.Code) > 128 {
		return false
	}
	switch r.Status {
	case "applied", "already_applied", "already_resolved":
		return r.AppliedAt != nil && !r.AppliedAt.IsZero() && r.Code == ""
	case "rejected":
		return r.AppliedAt == nil && r.Code != ""
	case "expired":
		return r.AppliedAt == nil
	default:
		return false
	}
}

func sameCommandOutcome(a, b domain.ProbeCommandOutcome) bool {
	return a.CommandID == b.CommandID && a.Status == b.Status && a.Code == b.Code && a.Message == b.Message && a.CredentialVersion == b.CredentialVersion && (a.AppliedAt == nil) == (b.AppliedAt == nil) && (a.AppliedAt == nil || a.AppliedAt.Equal(*b.AppliedAt))
}

func probeCommandError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	for _, known := range []error{ports.ErrConflict, ports.ErrNotFound, domain.ErrValidation, domain.ErrProbeKeyMismatch} {
		if errors.Is(err, known) {
			return known
		}
	}
	return errors.New("probe command storage failed")
}
