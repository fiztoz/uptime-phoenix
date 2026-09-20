package repository

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxHubStreamResets = 1024

// ProbeStreamResetStore performs administrative epoch recovery on both engines.
// It never dials a peer, sends a provider request or claims source confirmation.
type ProbeStreamResetStore struct {
	db          *bun.DB
	protector   ports.ProbeConfigProtector
	credentials ports.ProbeCredentialProtector
	codec       ports.ProbeStreamResetCodec
}

var _ ports.ProbeStreamResetRepository = (*ProbeStreamResetStore)(nil)

// NewProbeStreamResetStore requires the existing verified installation key.
func NewProbeStreamResetStore(db *bun.DB, protector ports.ProbeConfigProtector, credentials ports.ProbeCredentialProtector, codec ports.ProbeStreamResetCodec) *ProbeStreamResetStore {
	return &ProbeStreamResetStore{db: db, protector: protector, credentials: credentials, codec: codec}
}

type probeStreamResetRow struct {
	bun.BaseModel                                                                `bun:"table:probe_stream_resets"`
	ResetID                                                                      string `bun:",pk"`
	HubID, ProbeID, PreviousStreamID, StreamID, EnrollmentID, Fingerprint        string
	CredentialVersion, CertificateVersion, HubCommittedSeq, ConnectionGeneration int64
	PreparedAt                                                                   time.Time
	State                                                                        string
	SourceReceipt                                                                []byte
	ActivatedAt, ConfirmedAt                                                     *time.Time
}

func (r probeStreamResetRow) plan() domain.ProbeStreamResetPlan {
	return domain.ProbeStreamResetPlan{ResetID: r.ResetID, HubID: r.HubID, ProbeID: r.ProbeID, PreviousStreamID: r.PreviousStreamID, StreamID: r.StreamID, EnrollmentID: r.EnrollmentID, Fingerprint: r.Fingerprint, CredentialVersion: r.CredentialVersion, CertificateVersion: r.CertificateVersion, HubCommittedSeq: r.HubCommittedSeq, ConnectionGeneration: r.ConnectionGeneration, PreparedAt: r.PreparedAt.UTC()}
}

func (s *ProbeStreamResetStore) operation(ctx context.Context, row probeStreamResetRow) (*domain.ProbeStreamResetOperation, error) {
	p := row.plan()
	if !domain.ValidProbeStreamResetPlan(p) {
		return nil, domain.ErrValidation
	}
	out := &domain.ProbeStreamResetOperation{Plan: p, State: row.State, ActivatedAt: utcTimePtr(row.ActivatedAt), ConfirmedAt: utcTimePtr(row.ConfirmedAt)}
	switch row.State {
	case "prepared":
		if len(row.SourceReceipt) != 0 || row.ActivatedAt != nil || row.ConfirmedAt != nil {
			return nil, ports.ErrConflict
		}
	case "awaiting_peer", "complete":
		if row.ActivatedAt == nil || (row.State == "complete") != (row.ConfirmedAt != nil) {
			return nil, ports.ErrConflict
		}
		receipt, err := s.codec.DecodeStreamResetReceipt(ctx, row.SourceReceipt)
		if err != nil || receipt.Plan != p {
			return nil, ports.ErrConflict
		}
		out.Source = &receipt
	default:
		return nil, ports.ErrConflict
	}
	return out, nil
}

func (s *ProbeStreamResetStore) valid() bool {
	return s != nil && s.db != nil && s.protector != nil && s.credentials != nil && s.codec != nil
}

func (s *ProbeStreamResetStore) lockTarget(ctx context.Context, tx bun.Tx, hubID, probeID string) (probeRegistrationModel, error) {
	if _, err := tx.ExecContext(ctx, "UPDATE probes SET id = id WHERE id = ?", probeID); err != nil {
		return probeRegistrationModel{}, err
	}
	var registration probeRegistrationModel
	if err := tx.NewSelect().Model(&registration).Where("id = ?", probeID).Scan(ctx); err != nil {
		return registration, err
	}
	if registration.Kind != domain.ProbeKindRemote {
		return registration, ports.ErrConflict
	}
	var installation probeInstallationModel
	if err := tx.NewSelect().Model(&installation).Where("id = 1").Scan(ctx); err != nil {
		return registration, err
	}
	if installation.HubID != hubID || installation.KeyHash != s.protector.KeyHash(hubID) {
		return registration, domain.ErrProbeKeyMismatch
	}
	return registration, nil
}

// PrepareStreamReset snapshots the original cursor, reserves a never-used epoch,
// and revokes runtime/session authority in the same transaction as the operation.
func (s *ProbeStreamResetStore) PrepareStreamReset(ctx context.Context, issue domain.ProbeStreamResetIssue) (*domain.ProbeStreamResetOperation, error) {
	if !s.valid() || !domain.ValidProbeStreamResetIssue(issue) {
		return nil, domain.ErrValidation
	}
	var out *domain.ProbeStreamResetOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		registration, err := s.lockTarget(ctx, tx, issue.HubID, issue.ProbeID)
		if err != nil {
			return err
		}
		var old probeStreamResetRow
		err = tx.NewSelect().Model(&old).Where("reset_id = ?", issue.ResetID).Scan(ctx)
		if err == nil {
			if old.HubID != issue.HubID || old.ProbeID != issue.ProbeID || old.PreviousStreamID != issue.PreviousStreamID || old.StreamID != issue.StreamID {
				return ports.ErrConflict
			}
			out, err = s.operation(ctx, old)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !registration.Enabled {
			return ports.ErrConflict
		}
		if err := requireNoStreamReset(ctx, tx, issue.ProbeID, true); err != nil {
			return err
		}
		var count int
		if err := tx.NewRaw("SELECT COUNT(*) FROM probe_stream_resets WHERE probe_id = ?", issue.ProbeID).Scan(ctx, &count); err != nil {
			return err
		}
		if count >= maxHubStreamResets {
			return ports.ErrConflict
		}
		if err := tx.NewRaw("SELECT (SELECT COUNT(*) FROM probe_streams WHERE stream_id = ?) + (SELECT COUNT(*) FROM probe_stream_resets WHERE stream_id = ? OR previous_stream_id = ?)", issue.StreamID, issue.StreamID, issue.StreamID).Scan(ctx, &count); err != nil {
			return err
		}
		if count != 0 {
			return ports.ErrConflict
		}
		var connection probeConnectionRow
		if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", issue.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if connection.HubID != issue.HubID || connection.StreamID != issue.PreviousStreamID || connection.State != "active" {
			return ports.ErrConflict
		}
		if _, err := s.credentials.OpenCredential(ctx, connection.connection().ProbeCredentialMetadata, connection.ProtectedCredential); err != nil {
			return err
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		for _, table := range []string{"probe_credential_rotations", "probe_certificate_rotations"} {
			// Only certificate overlap has an independently persisted closed bit.
			// Credential overlap is bounded by its original immutable deadline.
			overlap := "overlap_expires_at > ?"
			if table == "probe_certificate_rotations" {
				overlap = "overlap_closed = FALSE AND " + overlap
			}
			if err := tx.NewRaw("SELECT COUNT(*) FROM "+table+" WHERE probe_id = ? AND (state IN ('preparing','activating') OR ("+overlap+"))", issue.ProbeID, now).Scan(ctx, &count); err != nil {
				return err
			}
			if count != 0 {
				return ports.ErrConflict
			}
		}
		var stream probeStreamModel
		if err := tx.NewSelect().Model(&stream).Where("probe_id = ? AND stream_id = ?", issue.ProbeID, issue.PreviousStreamID).Scan(ctx); err != nil {
			return err
		}
		if stream.RetiredAt != nil {
			return ports.ErrConflict
		}
		generation, err := reserveResetFence(ctx, tx, issue.ProbeID)
		if err != nil {
			return err
		}
		row := probeStreamResetRow{ResetID: issue.ResetID, HubID: issue.HubID, ProbeID: issue.ProbeID, PreviousStreamID: issue.PreviousStreamID, StreamID: issue.StreamID, EnrollmentID: connection.EnrollmentID, Fingerprint: connection.Fingerprint, CredentialVersion: connection.CredentialVersion, CertificateVersion: connection.CertificateVersion, HubCommittedSeq: stream.CommittedSeq, ConnectionGeneration: generation, PreparedAt: now, State: "prepared"}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
		out, err = s.operation(ctx, row)
		return err
	})
	if err != nil {
		return nil, probeStreamResetError(ctx, err)
	}
	return out, nil
}

func reserveResetFence(ctx context.Context, tx bun.Tx, probeID string) (int64, error) {
	session, err := readProbeSession(ctx, tx, probeID)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return 0, err
	}
	if session.Generation == math.MaxInt64 {
		return 0, ports.ErrConflict
	}
	session.ProbeID, session.OwnerID, session.LeaseUntil, session.Connected = probeID, "", 0, false
	session.Generation++
	if missing {
		_, err = tx.NewInsert().Model(&session).Exec(ctx)
	} else {
		_, err = tx.NewUpdate().Model(&session).WherePK().Exec(ctx)
	}
	if err != nil {
		return 0, err
	}
	parent, err := readProbeRuntime(ctx, tx, probeID)
	if errors.Is(err, sql.ErrNoRows) {
		return session.Generation, nil
	}
	if err != nil {
		return 0, err
	}
	if parent.Epoch == math.MaxInt64 {
		return 0, ports.ErrConflict
	}
	parent.Epoch++
	parent.OwnerID, parent.LeaseUntil = "", 0
	_, err = tx.NewUpdate().Model(&parent).WherePK().Exec(ctx)
	return session.Generation, err
}

// requireNoStreamReset must run under the probe authority lock when admitting a
// writer. Runtime admission is allowed after activation; new commands/rotations
// wait for actual peer confirmation so uncertain transitions cannot overlap.
func requireNoStreamReset(ctx context.Context, db bun.IDB, probeID string, includeAwaiting bool) error {
	query := "SELECT COUNT(*) FROM probe_stream_resets WHERE probe_id = ? AND state = 'prepared'"
	if includeAwaiting {
		query = "SELECT COUNT(*) FROM probe_stream_resets WHERE probe_id = ? AND state <> 'complete'"
	}
	var count int
	if err := db.NewRaw(query, probeID).Scan(ctx, &count); err != nil {
		return err
	}
	if count != 0 {
		return ports.ErrConflict
	}
	return nil
}

// GetStreamReset reads original nonsecret operation metadata under installation
// authority. It does not mutate the operation or infer peer admission.
func (s *ProbeStreamResetStore) GetStreamReset(ctx context.Context, hubID, probeID, resetID string) (*domain.ProbeStreamResetOperation, error) {
	if !s.valid() || !domain.ValidHubID(hubID) || !validRemoteProbeID(probeID) || !domain.ValidHubID(resetID) {
		return nil, domain.ErrValidation
	}
	var out *domain.ProbeStreamResetOperation
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		if _, err := s.lockTarget(ctx, tx, hubID, probeID); err != nil {
			return err
		}
		var row probeStreamResetRow
		if err := tx.NewSelect().Model(&row).Where("hub_id = ? AND probe_id = ? AND reset_id = ?", hubID, probeID, resetID).Scan(ctx); err != nil {
			return err
		}
		var err error
		out, err = s.operation(ctx, row)
		return err
	})
	if err != nil {
		return nil, probeStreamResetError(ctx, err)
	}
	return out, nil
}

// ActivateStreamReset accepts explicit operator evidence of source commit and
// atomically reseals the original token, retires the old stream and clears live
// evidence. Awaiting-peer is not a claim that the source is currently reachable.
func (s *ProbeStreamResetStore) ActivateStreamReset(ctx context.Context, receipt domain.EdgeStreamResetRecord) (*domain.ProbeStreamResetOperation, error) {
	if !s.valid() || !domain.ValidEdgeStreamResetRecord(receipt) || receipt.State != "applied" {
		return nil, domain.ErrValidation
	}
	data, err := s.codec.EncodeStreamResetReceipt(ctx, receipt)
	if err != nil {
		return nil, err
	}
	// Normalize all host-provided wall clocks through the actual closed codec.
	receipt, err = s.codec.DecodeStreamResetReceipt(ctx, data)
	if err != nil {
		return nil, err
	}
	var out *domain.ProbeStreamResetOperation
	err = runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		out = nil
		p := receipt.Plan
		registration, err := s.lockTarget(ctx, tx, p.HubID, p.ProbeID)
		if err != nil {
			return err
		}
		var row probeStreamResetRow
		if err := tx.NewSelect().Model(&row).Where("reset_id = ?", p.ResetID).Scan(ctx); err != nil {
			return err
		}
		if row.plan() != p {
			return ports.ErrConflict
		}
		if row.State != "prepared" {
			if !bytes.Equal(row.SourceReceipt, data) {
				return ports.ErrConflict
			}
			out, err = s.operation(ctx, row)
			return err
		}
		if !registration.Enabled {
			return ports.ErrConflict
		}
		var connection probeConnectionRow
		if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", p.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if connection.HubID != p.HubID || connection.StreamID != p.PreviousStreamID || connection.EnrollmentID != p.EnrollmentID || connection.CredentialVersion != p.CredentialVersion || connection.CertificateVersion != p.CertificateVersion || connection.Fingerprint != p.Fingerprint || connection.State != "active" {
			return ports.ErrConflict
		}
		token, err := s.credentials.OpenCredential(ctx, connection.connection().ProbeCredentialMetadata, connection.ProtectedCredential)
		if err != nil {
			return err
		}
		metadata := connection.connection().ProbeCredentialMetadata
		metadata.StreamID = p.StreamID
		protected, err := s.credentials.SealCredential(ctx, metadata, token)
		if err != nil {
			return err
		}
		lease, err := readProbeSession(ctx, tx, p.ProbeID)
		if err != nil {
			return err
		}
		if lease.Generation != p.ConnectionGeneration || lease.OwnerID != "" || lease.LeaseUntil != 0 || lease.Connected {
			return ports.ErrConflict
		}
		var old probeStreamModel
		if err := tx.NewSelect().Model(&old).Where("probe_id = ? AND stream_id = ?", p.ProbeID, p.PreviousStreamID).Scan(ctx); err != nil {
			return err
		}
		if old.RetiredAt != nil || old.CommittedSeq != p.HubCommittedSeq {
			return ports.ErrConflict
		}
		now, err := replayDatabaseTime(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE probe_streams SET retired_at = ?, updated_at = ? WHERE probe_id = ? AND stream_id = ?", now, now, p.ProbeID, p.PreviousStreamID); err != nil {
			return err
		}
		stream := probeStreamModel{ProbeID: p.ProbeID, StreamID: p.StreamID, CommittedSeq: 0, CreatedAt: now, UpdatedAt: now}
		if _, err := tx.NewInsert().Model(&stream).Exec(ctx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE probe_connections SET stream_id = ?, protected_credential = ? WHERE probe_id = ?", p.StreamID, protected, p.ProbeID); err != nil {
			return err
		}
		if err := invalidateResetProjection(ctx, tx, receipt, now); err != nil {
			return err
		}
		// Keep the original body, scope and any remote receipt. Local cancellation
		// can never prove what an unreachable copied source already executed.
		if _, err := tx.ExecContext(ctx, "UPDATE probe_commands SET status = 'canceled', next_attempt_at = NULL, result_code = 'stream_reset_unconfirmed', result_message = 'Local stream retirement; remote outcome unconfirmed', updated_at = ? WHERE probe_id = ? AND stream_id = ? AND remote_confirmed = ? AND status IN ('pending','blocked')", now, p.ProbeID, p.PreviousStreamID, false); err != nil {
			return err
		}
		row.State, row.SourceReceipt, row.ActivatedAt = "awaiting_peer", data, &now
		if _, err := tx.NewUpdate().Model(&row).WherePK().Exec(ctx); err != nil {
			return err
		}
		out, err = s.operation(ctx, row)
		return err
	})
	if err != nil {
		return nil, probeStreamResetError(ctx, err)
	}
	return out, nil
}

func invalidateResetProjection(ctx context.Context, tx bun.Tx, receipt domain.EdgeStreamResetRecord, now time.Time) error {
	p := receipt.Plan
	var assignments []struct{ MonitorID, Generation int64 }
	if err := tx.NewRaw("SELECT monitor_id, generation FROM monitor_probe_assignments WHERE probe_id = ? AND active = ? ORDER BY monitor_id", p.ProbeID, true).Scan(ctx, &assignments); err != nil {
		return err
	}
	for _, a := range assignments {
		if _, err := tx.ExecContext(ctx, "UPDATE monitor_probe_assignment_sets SET revision = revision WHERE monitor_id = ?", a.MonitorID); err != nil {
			return err
		}
	}
	for _, table := range []string{"monitor_probe_state", "probe_missing_state", "monitor_conditions", "tls_info"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE probe_id = ?", p.ProbeID); err != nil {
			return err
		}
	}
	for _, a := range assignments {
		row := probeMissingStateModel{MonitorID: a.MonitorID, ProbeID: p.ProbeID, StreamID: p.StreamID, AssignmentGeneration: a.Generation, ConfigRevision: receipt.Source.ConfigRevision, SnapshotSeq: 0, CreatedAt: now, AppliedAt: now, Reason: "stream_reset"}
		if _, err := tx.NewInsert().Model(&row).Exec(ctx); err != nil {
			return err
		}
	}
	return nil
}

// ConfirmStreamReset runs only after transport authenticates the selected pin,
// credential and new-stream health. It rechecks all DB authority through commit.
func (s *ProbeStreamResetStore) ConfirmStreamReset(ctx context.Context, session domain.ProbeReplaySession, metadata domain.ProbeCredentialMetadata, pin string) error {
	if !s.valid() || !validCommandSession(session) || !domain.ValidProbeCredentialMetadata(metadata) || metadata.HubID != session.HubID || metadata.ProbeID != session.ProbeID || metadata.StreamID != session.StreamID || !domain.ValidKeyHash(pin) {
		return domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		a, err := lockProbeSession(ctx, tx, session, s.protector.KeyHash(session.HubID))
		if err != nil {
			return err
		}
		var row probeStreamResetRow
		err = tx.NewSelect().Model(&row).Where("probe_id = ? AND stream_id = ?", session.ProbeID, session.StreamID).Scan(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return commandLeaseCovers(ctx, tx, a.lease, 0)
		}
		if err != nil {
			return err
		}
		if _, err := s.operation(ctx, row); err != nil {
			return err
		}
		if row.State == "complete" {
			return commandLeaseCovers(ctx, tx, a.lease, 0)
		}
		if row.State != "awaiting_peer" || session.ConnectionGeneration <= row.ConnectionGeneration {
			return ports.ErrConflict
		}
		var connection probeConnectionRow
		if err := tx.NewSelect().Model(&connection).Where("probe_id = ?", session.ProbeID).Scan(ctx); err != nil {
			return err
		}
		if connection.connection().ProbeCredentialMetadata != metadata || connection.State != "active" || pin != row.Fingerprint || metadata.Fingerprint != row.Fingerprint || metadata.EnrollmentID != row.EnrollmentID || metadata.CredentialVersion != row.CredentialVersion || connection.CertificateVersion != row.CertificateVersion {
			return ports.ErrConflict
		}
		if _, err := s.credentials.OpenCredential(ctx, metadata, connection.ProtectedCredential); err != nil {
			return err
		}
		if _, err := tx.NewUpdate().Model(&row).Set("state = 'complete'").Set("confirmed_at = ?", a.now).WherePK().Exec(ctx); err != nil {
			return err
		}
		return commandLeaseCovers(ctx, tx, a.lease, 0)
	})
	return probeStreamResetError(ctx, err)
}

func probeStreamResetError(ctx context.Context, err error) error {
	if errors.Is(err, domain.ErrProbeKeyMismatch) {
		return domain.ErrProbeKeyMismatch
	}
	return probeConnectionError(ctx, err)
}
