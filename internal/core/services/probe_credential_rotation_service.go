package services

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeCredentialRotationService generates and protects one immutable rotation.
// Its caller supplies a stable operation ID and version; no secret is returned.
type ProbeCredentialRotationService struct {
	repo        ports.ProbeCredentialRotationRepository
	connections ports.ProbeConnectionRepository
	commands    ports.ProbeCommandProtector
	credentials ports.ProbeCredentialProtector
	codec       ports.ProbeCredentialCommandCodec
	now         func() time.Time
}

// NewProbeCredentialRotationService uses the installation key for both stored
// candidates and requests. The repository commits all three before dispatch.
func NewProbeCredentialRotationService(repo ports.ProbeCredentialRotationRepository, connections ports.ProbeConnectionRepository, commands ports.ProbeCommandProtector, credentials ports.ProbeCredentialProtector, codec ports.ProbeCredentialCommandCodec) (*ProbeCredentialRotationService, error) {
	if repo == nil || connections == nil || commands == nil || credentials == nil || codec == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeCredentialRotationService{repo: repo, connections: connections, commands: commands, credentials: credentials, codec: codec, now: time.Now}, nil
}

// Issue recovers the original operation on retry, including after activation or
// expiry. A different version under the same operation ID is a conflict.
func (s *ProbeCredentialRotationService) Issue(ctx context.Context, issue domain.ProbeCredentialRotationIssue) (*domain.ProbeCredentialRotation, error) {
	if !domain.ValidHubID(issue.HubID) || !domain.ValidHubID(issue.ProbeID) || !domain.ValidHubID(issue.RotationID) || issue.CredentialVersion < 1 {
		return nil, domain.ErrValidation
	}
	if out, err := s.recover(ctx, issue); !errors.Is(err, ports.ErrNotFound) {
		return out, err
	}
	current, err := s.connections.GetConnection(ctx, issue.ProbeID)
	if err != nil {
		return nil, err
	}
	if current.HubID != issue.HubID || current.State != "active" || issue.CredentialVersion <= current.CredentialVersion {
		return nil, ports.ErrConflict
	}
	m := current.ProbeCredentialMetadata
	m.CredentialVersion = issue.CredentialVersion
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("probe credential generation failed")
	}
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(random[:])
	clear(random[:])
	protected, err := s.credentials.SealCredential(ctx, m, token)
	if err != nil {
		return nil, err
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
	r := domain.ProtectedProbeCredentialRotation{ProbeCredentialRotation: domain.ProbeCredentialRotation{RotationID: issue.RotationID, Candidate: m, PreviousVersion: current.CredentialVersion, PrepareCommandID: prepareID, ActivateCommandID: activateID, CreatedAt: now, OverlapExpiresAt: now.Add(domain.ProbeCredentialOverlap), State: "preparing"}, ProtectedCredential: protected}
	command := domain.ProbeCredentialCommand{CommandID: prepareID, ProbeID: m.ProbeID, Kind: "credential.prepare", CreatedAt: now, ExpiresAt: r.OverlapExpiresAt, RotationID: issue.RotationID, CredentialVersion: m.CredentialVersion, TokenHash: sha256.Sum256([]byte(token)), OverlapExpiresAt: r.OverlapExpiresAt}
	r.PrepareCommand, err = s.protect(ctx, m, command, token)
	if err != nil {
		return nil, err
	}
	command.CommandID, command.Kind, command.TokenHash, command.OverlapExpiresAt = activateID, "credential.activate", [32]byte{}, time.Time{}
	r.ActivateCommand, err = s.protect(ctx, m, command, "")
	if err != nil {
		return nil, err
	}
	out, err := s.repo.CreateCredentialRotation(ctx, r)
	if errors.Is(err, ports.ErrConflict) {
		if saved, recoveryErr := s.recover(ctx, issue); recoveryErr == nil {
			return saved, nil
		}
	}
	return out, err
}

func (s *ProbeCredentialRotationService) recover(ctx context.Context, issue domain.ProbeCredentialRotationIssue) (*domain.ProbeCredentialRotation, error) {
	r, err := s.repo.GetCredentialRotation(ctx, issue.HubID, issue.ProbeID, issue.RotationID)
	if err != nil {
		return nil, err
	}
	if r.Candidate.HubID != issue.HubID || r.Candidate.ProbeID != issue.ProbeID || r.RotationID != issue.RotationID || r.Candidate.CredentialVersion != issue.CredentialVersion {
		return nil, ports.ErrConflict
	}
	return r, nil
}

func (s *ProbeCredentialRotationService) protect(ctx context.Context, target domain.ProbeCredentialMetadata, command domain.ProbeCredentialCommand, token string) (domain.ProtectedProbeCommand, error) {
	plain, err := s.codec.EncodeCredentialCommand(ctx, command, token)
	if err != nil {
		return domain.ProtectedProbeCommand{}, err
	}
	defer clear(plain)
	sum := sha256.Sum256(plain)
	m := domain.ProbeCommandMetadata{CommandID: command.CommandID, HubID: target.HubID, ProbeID: target.ProbeID, StreamID: target.StreamID, Kind: command.Kind, CreatedAt: command.CreatedAt, ExpiresAt: command.ExpiresAt, PayloadSHA256: hex.EncodeToString(sum[:])}
	protected, err := s.commands.SealCommand(ctx, m, plain)
	if err != nil {
		return domain.ProtectedProbeCommand{}, err
	}
	return domain.ProtectedProbeCommand{ProbeCommandMetadata: m, ProtectedPayload: protected}, nil
}
