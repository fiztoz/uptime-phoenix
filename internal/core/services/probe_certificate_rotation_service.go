package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeCertificateRotationService protects one stable certificate preparation.
// Certificate keys are generated only at the source, never at the hub.
type ProbeCertificateRotationService struct {
	repo        ports.ProbeCertificateRotationRepository
	connections ports.ProbeConnectionRepository
	protector   ports.ProbeCommandProtector
	codec       ports.ProbeCertificateCommandCodec
	now         func() time.Time
}

// NewProbeCertificateRotationService requires the installation protection key.
func NewProbeCertificateRotationService(repo ports.ProbeCertificateRotationRepository, connections ports.ProbeConnectionRepository, protector ports.ProbeCommandProtector, codec ports.ProbeCertificateCommandCodec) (*ProbeCertificateRotationService, error) {
	if repo == nil || connections == nil || protector == nil || codec == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeCertificateRotationService{repo: repo, connections: connections, protector: protector, codec: codec, now: time.Now}, nil
}

// Issue persists exact preparation bytes before dispatch. Matching retries
// recover the original IDs, timestamps and state even after the overlap expires.
func (s *ProbeCertificateRotationService) Issue(ctx context.Context, issue domain.ProbeCertificateRotationIssue) (*domain.ProbeCertificateRotation, error) {
	if !domain.ValidHubID(issue.HubID) || !domain.ValidHubID(issue.ProbeID) || !domain.ValidHubID(issue.RotationID) || issue.CertificateVersion <= 1 || issue.ValidForDays < 1 || issue.ValidForDays > 3650 {
		return nil, domain.ErrValidation
	}
	if r, err := s.recover(ctx, issue); !errors.Is(err, ports.ErrNotFound) {
		return r, err
	}
	current, err := s.connections.GetConnection(ctx, issue.ProbeID)
	if err != nil {
		return nil, err
	}
	if current.HubID != issue.HubID || current.State != "active" || current.CertificateVersion < 1 || issue.CertificateVersion <= current.CertificateVersion {
		return nil, ports.ErrConflict
	}
	prepareID, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	activateID, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	r := domain.ProtectedProbeCertificateRotation{ProbeCertificateRotation: domain.ProbeCertificateRotation{RotationID: issue.RotationID, Current: current.ProbeCredentialMetadata, CertificateVersion: issue.CertificateVersion, PreviousVersion: current.CertificateVersion, ValidForDays: issue.ValidForDays, PrepareCommandID: prepareID, ActivateCommandID: activateID, CreatedAt: now, OverlapExpiresAt: now.Add(domain.ProbeCredentialOverlap), State: "preparing"}}
	c := domain.ProbeCertificateCommand{CommandID: prepareID, ProbeID: issue.ProbeID, Kind: "certificate.prepare", CreatedAt: now, ExpiresAt: r.OverlapExpiresAt, RotationID: issue.RotationID, CertificateVersion: issue.CertificateVersion, ValidForDays: issue.ValidForDays}
	plain, err := s.codec.EncodeCertificateCommand(ctx, c)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	sum := sha256.Sum256(plain)
	m := domain.ProbeCommandMetadata{CommandID: prepareID, HubID: issue.HubID, ProbeID: issue.ProbeID, StreamID: current.StreamID, Kind: c.Kind, CreatedAt: now, ExpiresAt: r.OverlapExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	cipher, err := s.protector.SealCommand(ctx, m, plain)
	if err != nil {
		return nil, err
	}
	r.PrepareCommand = domain.ProtectedProbeCommand{ProbeCommandMetadata: m, ProtectedPayload: cipher}
	out, err := s.repo.CreateCertificateRotation(ctx, r)
	if errors.Is(err, ports.ErrConflict) {
		if saved, recoveryErr := s.recover(ctx, issue); recoveryErr == nil {
			return saved, nil
		}
	}
	return out, err
}

func (s *ProbeCertificateRotationService) recover(ctx context.Context, issue domain.ProbeCertificateRotationIssue) (*domain.ProbeCertificateRotation, error) {
	r, err := s.repo.GetCertificateRotation(ctx, issue.HubID, issue.ProbeID, issue.RotationID)
	if err != nil {
		return nil, err
	}
	if r.Current.HubID != issue.HubID || r.Current.ProbeID != issue.ProbeID || r.RotationID != issue.RotationID || r.CertificateVersion != issue.CertificateVersion || r.ValidForDays != issue.ValidForDays {
		return nil, ports.ErrConflict
	}
	return r, nil
}
