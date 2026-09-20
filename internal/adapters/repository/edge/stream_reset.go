package edge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const maxEdgeStreamResets = 16

// resetRecordDTO is a bounded private persistence format, never a wire response.
// Domain structs deliberately have no JSON tags and are not serialized directly.
type resetRecordDTO struct {
	Version                    int       `json:"version"`
	ResetID                    string    `json:"reset_id"`
	HubID                      string    `json:"hub_id"`
	ProbeID                    string    `json:"probe_id"`
	EnrollmentID               string    `json:"enrollment_id"`
	PreviousStreamID           string    `json:"previous_stream_id"`
	StreamID                   string    `json:"stream_id"`
	Fingerprint                string    `json:"fingerprint"`
	CredentialVersion          int64     `json:"credential_version"`
	CertificateVersion         int64     `json:"certificate_version"`
	HubCommittedSeq            int64     `json:"hub_committed_seq"`
	ConnectionGeneration       int64     `json:"connection_generation"`
	PreparedAt                 time.Time `json:"prepared_at"`
	InitialStreamID            string    `json:"initial_stream_id"`
	SourceFingerprint          string    `json:"source_fingerprint"`
	SourceLastCreatedSeq       int64     `json:"source_last_created_seq"`
	SourceCommittedSeq         int64     `json:"source_committed_seq"`
	SourceConnectionGeneration int64     `json:"source_connection_generation"`
	SourceConfigRevision       int64     `json:"source_config_revision"`
	State                      string    `json:"state"`
	ReservedAt                 time.Time `json:"reserved_at"`
	AppliedAt                  time.Time `json:"applied_at"`
	ArchiveBytes               int64     `json:"archive_bytes"`
	ArchiveSHA256              string    `json:"archive_sha256"`
}

func encodeResetRecord(r domain.EdgeStreamResetRecord) ([]byte, error) {
	if !domain.ValidEdgeStreamResetRecord(r) {
		return nil, domain.ErrValidation
	}
	p, s := r.Plan, r.Source
	return json.Marshal(resetRecordDTO{Version: 1, ResetID: p.ResetID, HubID: p.HubID, ProbeID: p.ProbeID, EnrollmentID: p.EnrollmentID, PreviousStreamID: p.PreviousStreamID, StreamID: p.StreamID, Fingerprint: p.Fingerprint, CredentialVersion: p.CredentialVersion, CertificateVersion: p.CertificateVersion, HubCommittedSeq: p.HubCommittedSeq, ConnectionGeneration: p.ConnectionGeneration, PreparedAt: p.PreparedAt.UTC(), InitialStreamID: r.InitialStreamID, SourceFingerprint: s.Fingerprint, SourceLastCreatedSeq: s.LastCreatedSeq, SourceCommittedSeq: s.CommittedSeq, SourceConnectionGeneration: s.ConnectionGeneration, SourceConfigRevision: s.ConfigRevision, State: r.State, ReservedAt: r.ReservedAt.UTC(), AppliedAt: r.AppliedAt.UTC(), ArchiveBytes: r.ArchiveBytes, ArchiveSHA256: r.ArchiveSHA256})
}

func decodeResetRecord(data []byte) (domain.EdgeStreamResetRecord, error) {
	if len(data) == 0 || len(data) > 8192 {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	var d resetRecordDTO
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) || d.Version != 1 {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	r := domain.EdgeStreamResetRecord{
		Plan:            domain.ProbeStreamResetPlan{ResetID: d.ResetID, HubID: d.HubID, ProbeID: d.ProbeID, EnrollmentID: d.EnrollmentID, PreviousStreamID: d.PreviousStreamID, StreamID: d.StreamID, Fingerprint: d.Fingerprint, CredentialVersion: d.CredentialVersion, CertificateVersion: d.CertificateVersion, HubCommittedSeq: d.HubCommittedSeq, ConnectionGeneration: d.ConnectionGeneration, PreparedAt: d.PreparedAt.UTC()},
		InitialStreamID: d.InitialStreamID,
		Source:          domain.EdgeIdentity{ProbeID: d.ProbeID, HubID: d.HubID, StreamID: d.PreviousStreamID, Fingerprint: d.SourceFingerprint, LastCreatedSeq: d.SourceLastCreatedSeq, CommittedSeq: d.SourceCommittedSeq, ConnectionGeneration: d.SourceConnectionGeneration, ConfigRevision: d.SourceConfigRevision},
		State:           d.State, ReservedAt: d.ReservedAt.UTC(), AppliedAt: d.AppliedAt.UTC(), ArchiveBytes: d.ArchiveBytes, ArchiveSHA256: d.ArchiveSHA256,
	}
	// Canonical encoding also rejects duplicate keys and alternate interpretations.
	canonical, err := encodeResetRecord(r)
	if err != nil || !bytes.Equal(data, canonical) {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	return r, nil
}

type edgeResetRow struct {
	bun.BaseModel                     `bun:"table:edge_stream_resets"`
	ResetID                           string `bun:",pk"`
	PreviousStreamID, StreamID, State string
	Record, Proof                     []byte
}

func (s *Store) authenticateResetRow(ctx context.Context, row edgeResetRow) (domain.EdgeStreamResetRecord, error) {
	r, err := decodeResetRecord(row.Record)
	if err != nil || s.resetProof == nil || r.Plan.ResetID != row.ResetID || r.Plan.PreviousStreamID != row.PreviousStreamID || r.Plan.StreamID != row.StreamID || r.State != row.State {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := s.resetProof.VerifyStreamReset(ctx, r, row.Proof); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	return r, nil
}

func (s *Store) verifyStreamResetChain(ctx context.Context, anchor, current domain.EdgeIdentity) error {
	var initial string
	if err := s.db.NewRaw("SELECT initial_stream_id FROM edge_identity WHERE id = 1").Scan(ctx, &initial); err != nil {
		return err
	}
	if initial != anchor.StreamID {
		return ports.ErrConflict
	}
	var rows []edgeResetRow
	if err := s.db.NewSelect().Model(&rows).Limit(maxEdgeStreamResets + 1).Scan(ctx); err != nil {
		return err
	}
	if len(rows) > maxEdgeStreamResets {
		return ErrStorage
	}
	byPrevious := make(map[string]domain.EdgeStreamResetRecord, len(rows))
	for _, row := range rows {
		r, err := s.authenticateResetRow(ctx, row)
		if err != nil {
			return err
		}
		if r.InitialStreamID != initial || r.Source.ProbeID != anchor.ProbeID || r.Source.Fingerprint != anchor.Fingerprint || r.Source.HubID != current.HubID {
			return ErrStorage
		}
		byPrevious[r.Plan.PreviousStreamID] = r
	}
	stream := initial
	var generationFloor, revisionFloor int64
	visited := make(map[string]bool, len(rows)+1)
	for {
		if visited[stream] {
			return ErrStorage
		}
		visited[stream] = true
		r, ok := byPrevious[stream]
		if !ok {
			break
		}
		delete(byPrevious, stream)
		if r.Source.ConnectionGeneration < generationFloor || r.Source.ConfigRevision < revisionFloor {
			return ErrStorage
		}
		if r.State == "prepared" {
			if !s.resetMode || current != r.Source || len(byPrevious) != 0 {
				return ports.ErrConflict
			}
			break
		}
		if r.State != "applied" {
			return ErrStorage
		}
		stream = r.Plan.StreamID
		generationFloor, revisionFloor = r.Plan.ConnectionGeneration, r.Source.ConfigRevision
	}
	if len(byPrevious) != 0 || stream != current.StreamID || current.ConnectionGeneration < generationFloor || current.ConfigRevision < revisionFloor {
		return ports.ErrConflict
	}
	return nil
}

// ResetStream preserves old evidence before atomically selecting the new epoch.
// It requires OpenForStreamReset and the immutable plan already prepared by the
// hub administrator. A durable reservation blocks normal restarts after failure;
// retry the exact plan to finish. No network or provider work happens here.
func (s *Store) ResetStream(ctx context.Context, plan domain.ProbeStreamResetPlan) (domain.EdgeStreamResetRecord, error) {
	s.resetMu.Lock()
	defer s.resetMu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	if !s.resetMode || s.resetProof == nil || s.resetConfig == nil || !domain.ValidProbeStreamResetPlan(plan) {
		return domain.EdgeStreamResetRecord{}, domain.ErrValidation
	}
	plan.PreparedAt = plan.PreparedAt.UTC()
	r, err := s.reserveStreamReset(ctx, plan)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	if r.State == "applied" {
		if _, err := s.verifyResetArchive(ctx, r); err != nil {
			return domain.EdgeStreamResetRecord{}, err
		}
		return r, nil
	}
	archived, err := s.archiveForStreamReset(ctx, r)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	return s.commitStreamReset(ctx, archived)
}

func (s *Store) reserveStreamReset(ctx context.Context, plan domain.ProbeStreamResetPlan) (domain.EdgeStreamResetRecord, error) {
	var result domain.EdgeStreamResetRecord
	err := s.writeRecovery(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		var old edgeResetRow
		err := tx.NewSelect().Model(&old).Where("reset_id = ?", plan.ResetID).Scan(ctx)
		if err == nil {
			r, err := s.authenticateResetRow(ctx, old)
			if err != nil {
				return err
			}
			if r.Plan != plan {
				return ports.ErrConflict
			}
			result = r
			return nil
		}
		if !errors.Is(storageError(ctx, err), ports.ErrNotFound) {
			return err
		}
		if identity.ProbeID != plan.ProbeID || identity.HubID != plan.HubID || identity.StreamID != plan.PreviousStreamID || identity.ConnectionGeneration >= plan.ConnectionGeneration {
			return ports.ErrConflict
		}
		var count int
		if err := tx.NewRaw("SELECT COUNT(*) FROM edge_stream_resets").Scan(ctx, &count); err != nil {
			return err
		}
		if count >= maxEdgeStreamResets {
			return ErrQueueFull
		}
		if err := tx.NewRaw("SELECT COUNT(*) FROM edge_stream_resets WHERE state = 'prepared' OR previous_stream_id = ? OR stream_id = ?", plan.StreamID, plan.StreamID).Scan(ctx, &count); err != nil {
			return err
		}
		if count != 0 {
			return ports.ErrConflict
		}
		var initial string
		if err := tx.NewRaw("SELECT initial_stream_id FROM edge_identity WHERE id = 1").Scan(ctx, &initial); err != nil {
			return err
		}
		if initial == plan.StreamID {
			return ports.ErrConflict
		}
		now := s.commandNow().UTC().Truncate(time.Microsecond)
		if err := s.validateResetMaterial(ctx, tx, identity, plan, now); err != nil {
			return err
		}
		result = domain.EdgeStreamResetRecord{Plan: plan, InitialStreamID: initial, Source: identity, State: "prepared", ReservedAt: now}
		data, err := encodeResetRecord(result)
		if err != nil {
			return err
		}
		proof, err := s.resetProof.SealStreamReset(ctx, result)
		if err != nil {
			return err
		}
		row := edgeResetRow{ResetID: plan.ResetID, PreviousStreamID: plan.PreviousStreamID, StreamID: plan.StreamID, State: result.State, Record: data, Proof: proof}
		_, err = tx.NewInsert().Model(&row).Exec(ctx)
		return err
	})
	return result, err
}

func (s *Store) validateResetMaterial(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity, p domain.ProbeStreamResetPlan, now time.Time) error {
	var credential struct {
		HubID, EnrollmentID string
		Version             int64
	}
	if err := tx.NewRaw("SELECT hub_id, enrollment_id, version FROM edge_credentials WHERE kind = 'runtime'").Scan(ctx, &credential); err != nil {
		return err
	}
	if credential.HubID != p.HubID || credential.EnrollmentID != p.EnrollmentID || credential.Version != p.CredentialVersion {
		return ports.ErrConflict
	}
	if err := expireCredentialOverlaps(ctx, tx, now); err != nil {
		return err
	}
	if err := expireCertificateOverlaps(ctx, tx, now); err != nil {
		return err
	}
	var count int
	if err := tx.NewRaw("SELECT (SELECT COUNT(*) FROM edge_credential_rotations WHERE overlap_closed = 0) + (SELECT COUNT(*) FROM edge_certificate_rotations WHERE overlap_closed = 0)").Scan(ctx, &count); err != nil {
		return err
	}
	if count != 0 {
		return ports.ErrConflict
	}
	state, err := readCertificateState(ctx, tx)
	if err != nil {
		return err
	}
	if state.ActiveVersion != p.CertificateVersion {
		return ports.ErrConflict
	}
	if state.ActiveVersion == 1 {
		if p.Fingerprint != i.Fingerprint {
			return ports.ErrConflict
		}
	} else {
		var row edgeCertificateRotation
		if err := tx.NewSelect().Model(&row).Where("version = ?", state.ActiveVersion).Scan(ctx); err != nil {
			return err
		}
		if row.ActivatedAt == nil || row.Fingerprint != p.Fingerprint || row.StreamID != i.StreamID || row.ProbeID != i.ProbeID || row.HubID != i.HubID || s.certificates == nil {
			return ports.ErrConflict
		}
		if err := s.certificates.ValidateCertificate(ctx, row.protected(), now); err != nil {
			return err
		}
	}
	if i.ConfigRevision > 0 {
		config, err := readActiveConfig(ctx, tx)
		if err != nil {
			return err
		}
		plain, err := s.resetConfig.Open(ctx, config.Snapshot.ProbeConfigMetadata, config.Snapshot.ProtectedPayload)
		clear(plain)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) commitStreamReset(ctx context.Context, archived domain.EdgeStreamResetRecord) (domain.EdgeStreamResetRecord, error) {
	result := archived
	result.State, result.AppliedAt = "applied", s.commandNow().UTC().Truncate(time.Microsecond)
	err := s.writeRecovery(ctx, func(ctx context.Context, tx bun.Tx, identity domain.EdgeIdentity) error {
		var row edgeResetRow
		if err := tx.NewSelect().Model(&row).Where("reset_id = ?", archived.Plan.ResetID).Scan(ctx); err != nil {
			return err
		}
		pending, err := s.authenticateResetRow(ctx, row)
		if err != nil {
			return err
		}
		expected := archived
		expected.State, expected.ArchiveBytes, expected.ArchiveSHA256 = "prepared", 0, ""
		if pending != expected || identity != pending.Source {
			return ports.ErrConflict
		}
		if err := s.validateResetMaterial(ctx, tx, identity, pending.Plan, result.AppliedAt); err != nil {
			return err
		}
		if pending.Plan.CertificateVersion > 1 {
			material, ok := s.certificates.(ports.EdgeCertificateRebinder)
			if !ok {
				return domain.ErrValidation
			}
			var certificate edgeCertificateRotation
			if err := tx.NewSelect().Model(&certificate).Where("version = ?", pending.Plan.CertificateVersion).Scan(ctx); err != nil {
				return err
			}
			rebound, err := material.RebindCertificate(ctx, certificate.protected(), pending.Plan.StreamID, result.AppliedAt)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE edge_certificate_rotations SET stream_id = ?, protected_pem = ? WHERE version = ?", rebound.StreamID, rebound.ProtectedPEM, rebound.Version); err != nil {
				return err
			}
		}
		// History survives in the authenticated archive under its original epoch.
		// Retain immutable command hashes and rotation high-water in the active DB.
		for _, table := range []string{"edge_watchdog_state", "edge_delivery_outbox", "edge_regional_state", "edge_alerts", "edge_telemetry_outbox", "edge_gaps"} {
			if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET stream_id = ?, last_created_seq = 0, committed_seq = 0, connection_generation = ? WHERE id = 1", pending.Plan.StreamID, pending.Plan.ConnectionGeneration); err != nil {
			return err
		}
		data, err := encodeResetRecord(result)
		if err != nil {
			return err
		}
		proof, err := s.resetProof.SealStreamReset(ctx, result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE edge_stream_resets SET state = 'applied', record = ?, proof = ? WHERE reset_id = ?", data, proof, pending.Plan.ResetID)
		return err
	})
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	return result, nil
}
