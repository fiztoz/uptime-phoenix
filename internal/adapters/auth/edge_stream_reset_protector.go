package auth

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.EdgeStreamResetProtector = (*ProbeConfigProtector)(nil)

// SealStreamReset binds the full immutable recovery record without exposing keys.
func (p *ProbeConfigProtector) SealStreamReset(ctx context.Context, r domain.EdgeStreamResetRecord) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p == nil || p.aead == nil || !domain.ValidEdgeStreamResetRecord(r) {
		return nil, domain.ErrValidation
	}
	return p.aead.Seal([]byte{1}, nil, nil, streamResetAssociatedData(r)), nil
}

// VerifyStreamReset authenticates every phase, archive digest and identity bound.
func (p *ProbeConfigProtector) VerifyStreamReset(ctx context.Context, r domain.EdgeStreamResetRecord, proof []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p == nil || p.aead == nil || !domain.ValidEdgeStreamResetRecord(r) || len(proof) != domain.ProbeConfigProtectionOverhead || proof[0] != 1 {
		return domain.ErrValidation
	}
	if _, err := p.aead.Open(nil, nil, proof[1:], streamResetAssociatedData(r)); err != nil {
		return errors.New("stream reset authentication failed")
	}
	return nil
}

func streamResetAssociatedData(r domain.EdgeStreamResetRecord) []byte {
	p, s := r.Plan, r.Source
	data, _ := json.Marshal([]any{"phoenix.edge.stream-reset.aes256gcm.v1", p.ResetID, p.HubID, p.ProbeID, p.EnrollmentID, p.PreviousStreamID, p.StreamID, p.Fingerprint, p.CredentialVersion, p.CertificateVersion, p.HubCommittedSeq, p.ConnectionGeneration, p.PreparedAt.UTC().Format(time.RFC3339Nano), r.InitialStreamID, s.ProbeID, s.StreamID, s.Fingerprint, s.HubID, s.LastCreatedSeq, s.CommittedSeq, s.ConnectionGeneration, s.ConfigRevision, r.State, r.ReservedAt.UTC().Format(time.RFC3339Nano), r.AppliedAt.UTC().Format(time.RFC3339Nano), r.ArchiveBytes, r.ArchiveSHA256})
	return data
}
