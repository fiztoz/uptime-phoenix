package services

import (
	"context"
	"encoding/hex"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeCommandService issues and retries immutable requests. Operator authorization
// belongs to its inbound adapter; source replay authorization stays in AccessService.
type ProbeCommandService struct {
	repo            ports.ProbeCommandRepository
	connections     ports.ProbeConnectionRepository
	protector       ports.ProbeCommandProtector
	codec           ports.ProbeAcknowledgementCodec
	credentialCodec ports.ProbeCredentialCommandCodec
	now             func() time.Time
}

var _ ports.ProbeCommandDispatcher = (*ProbeCommandService)(nil)

// NewProbeCommandService wires the existing installation key and connection scope.
func NewProbeCommandService(repo ports.ProbeCommandRepository, connections ports.ProbeConnectionRepository, protector ports.ProbeCommandProtector, codec ports.ProbeAcknowledgementCodec, credentialCodec ports.ProbeCredentialCommandCodec) (*ProbeCommandService, error) {
	if repo == nil || connections == nil || protector == nil || codec == nil || credentialCodec == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeCommandService{repo: repo, connections: connections, protector: protector, codec: codec, credentialCodec: credentialCodec, now: time.Now}, nil
}

// IssueAcknowledgement persists an operator's exact original-incident request.
// Retrying an explicit command ID compares semantic input before reusing its bytes.
func (s *ProbeCommandService) IssueAcknowledgement(ctx context.Context, issue domain.ProbeAcknowledgementIssue) (*domain.ProbeCommand, error) {
	if !domain.ValidHubID(issue.HubID) || !domain.ValidHubID(issue.ProbeID) || !domain.ValidHubID(issue.SourceAlertID) || issue.AssignmentGeneration < 1 || issue.Lifetime < time.Second || issue.Lifetime > 7*24*time.Hour {
		return nil, domain.ErrValidation
	}
	if issue.CommandID == "" {
		id, err := newUUIDv4()
		if err != nil {
			return nil, err
		}
		issue.CommandID = id
	} else if !domain.ValidHubID(issue.CommandID) {
		return nil, domain.ErrValidation
	}
	if existing, err := s.recoverIssuance(ctx, issue); !errors.Is(err, ports.ErrNotFound) {
		return existing, err
	}
	connection, err := s.connections.GetConnection(ctx, issue.ProbeID)
	if err != nil {
		return nil, err
	}
	if connection.HubID != issue.HubID || connection.State != "active" {
		return nil, ports.ErrConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	ack := domain.ProbeAlertAcknowledgement{CommandID: issue.CommandID, ProbeID: issue.ProbeID, SourceAlertID: issue.SourceAlertID, AssignmentGeneration: issue.AssignmentGeneration, CreatedAt: now, ExpiresAt: now.Add(issue.Lifetime), ActorDisplayName: issue.ActorDisplayName, Note: issue.Note}
	payload, err := s.codec.EncodeAcknowledgement(ctx, ack)
	if err != nil {
		return nil, err
	}
	defer clear(payload)
	decoded, err := s.codec.DecodeAcknowledgement(ctx, payload)
	if err != nil {
		return nil, err
	}
	meta := domain.ProbeCommandMetadata{CommandID: issue.CommandID, HubID: issue.HubID, ProbeID: issue.ProbeID, StreamID: connection.StreamID, Kind: "alert.ack", SourceAlertID: &issue.SourceAlertID, AssignmentGeneration: &issue.AssignmentGeneration, CreatedAt: ack.CreatedAt, ExpiresAt: ack.ExpiresAt, PayloadSHA256: hex.EncodeToString(decoded.PayloadHash[:])}
	protected, err := s.protector.SealCommand(ctx, meta, payload)
	if err != nil {
		return nil, err
	}
	out, err := s.repo.CreateCommand(ctx, domain.ProtectedProbeCommand{ProbeCommandMetadata: meta, ProtectedPayload: protected})
	if errors.Is(err, ports.ErrConflict) {
		// A simultaneous identical issuance may have won using its own timestamp
		// and AEAD nonce. Recover it only if every immutable operator input matches.
		if existing, recoveryErr := s.recoverIssuance(ctx, issue); recoveryErr == nil {
			return existing, nil
		}
	}
	return out, err
}

func (s *ProbeCommandService) recoverIssuance(ctx context.Context, issue domain.ProbeAcknowledgementIssue) (*domain.ProbeCommand, error) {
	stored, err := s.repo.GetProtectedCommand(ctx, issue.HubID, issue.ProbeID, issue.CommandID)
	if err != nil {
		return nil, err
	}
	plain, err := s.protector.OpenCommand(ctx, stored.ProbeCommandMetadata, stored.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	defer clear(plain)
	ack, err := s.codec.DecodeAcknowledgement(ctx, plain)
	if err != nil {
		return nil, err
	}
	if ack.CommandID != issue.CommandID || ack.ProbeID != issue.ProbeID || ack.SourceAlertID != issue.SourceAlertID || ack.AssignmentGeneration != issue.AssignmentGeneration || ack.ActorDisplayName != issue.ActorDisplayName || ack.ExpiresAt.Sub(ack.CreatedAt) != issue.Lifetime || (ack.Note == nil) != (issue.Note == nil) || ack.Note != nil && *ack.Note != *issue.Note {
		return nil, ports.ErrConflict
	}
	return s.repo.GetCommand(ctx, issue.HubID, issue.ProbeID, issue.CommandID)
}

// NextCommand returns a single due request after durable authority and retry state
// commit. A lost socket write reuses these same bytes on the next attempt.
func (s *ProbeCommandService) NextCommand(ctx context.Context, session domain.ProbeReplaySession, budget time.Duration, capabilities domain.ProbeCommandCapabilities) (*domain.ProbeCommandDispatch, error) {
	stored, err := s.repo.ClaimCommand(ctx, session, budget, capabilities)
	if err != nil || stored == nil {
		return nil, err
	}
	plain, err := s.protector.OpenCommand(ctx, stored.ProbeCommandMetadata, stored.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	switch stored.Kind {
	case "alert.ack":
		_, err = s.codec.DecodeAcknowledgement(ctx, plain)
	case "credential.prepare", "credential.activate":
		_, err = s.credentialCodec.DecodeCredentialCommand(ctx, plain)
	default:
		err = domain.ErrValidation
	}
	if err != nil {
		clear(plain)
		return nil, err
	}
	return &domain.ProbeCommandDispatch{Metadata: stored.ProbeCommandMetadata, Payload: plain}, nil
}

// RecordCommandResult confirms only a source receipt under current session authority.
func (s *ProbeCommandService) RecordCommandResult(ctx context.Context, session domain.ProbeReplaySession, result domain.ProbeCommandOutcome) (bool, error) {
	command, err := s.repo.GetCommand(ctx, session.HubID, session.ProbeID, result.CommandID)
	if err != nil {
		return false, err
	}
	if err := s.repo.CompleteCommand(ctx, session, result); err != nil {
		return false, err
	}
	return command.Kind == "credential.prepare" || command.Kind == "credential.activate", nil
}
