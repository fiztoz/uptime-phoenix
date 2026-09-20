package services

import (
	"context"
	"crypto/subtle"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// EdgeConfigService authenticates protected configuration on every cold load and
// activates only complete, validated documents under the current session fence.
type EdgeConfigService struct {
	identity  ports.EdgeIdentityRepository
	store     ports.EdgeConfigRepository
	decoder   ports.EdgeConfigDecoder
	protector ports.ProbeConfigProtector
}

// NewEdgeConfigService wires storage, bounded decoding and an explicitly loaded key.
func NewEdgeConfigService(identity ports.EdgeIdentityRepository, store ports.EdgeConfigRepository, decoder ports.EdgeConfigDecoder, protector ports.ProbeConfigProtector) *EdgeConfigService {
	return &EdgeConfigService{identity: identity, store: store, decoder: decoder, protector: protector}
}

// Apply validates and encrypts exact transferred bytes before a fenced commit.
func (s *EdgeConfigService) Apply(ctx context.Context, document []byte, generation int64, at time.Time) (*domain.EdgeResolvedConfig, error) {
	if generation <= 0 || at.IsZero() {
		return nil, domain.ErrValidation
	}
	i, err := s.identity.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if i.HubID == "" || generation != i.ConnectionGeneration {
		return nil, ports.ErrConflict
	}
	target := domain.ProbeConfigTarget{HubID: i.HubID, ProbeID: i.ProbeID}
	resolved, err := s.decoder.DecodeEdge(ctx, document, target)
	if err != nil {
		return nil, err
	}
	protected, err := s.protector.Seal(ctx, resolved.Metadata, document)
	if err != nil {
		return nil, err
	}
	indices := make([]domain.EdgeAssignmentIdentity, 0, len(resolved.Assignments))
	for _, a := range resolved.Assignments {
		indices = append(indices, domain.EdgeAssignmentIdentity{MonitorID: a.Monitor.ID, Generation: a.Generation, Active: a.Monitor.Active})
	}
	command := domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{ProbeConfigMetadata: resolved.Metadata, KeyConfirmation: s.protector.KeyHash(i.HubID), ProtectedPayload: protected}, Assignments: indices, AppliedAt: at.UTC(), ConnectionGeneration: generation}
	if err := s.store.ActivateConfig(ctx, command); err != nil {
		return nil, err
	}
	return resolved, nil
}

// Load authenticates exact active bytes and reruns the current build's validators.
// Wrong keys, unsupported settings and corrupt bytes never fall back to old config.
func (s *EdgeConfigService) Load(ctx context.Context) (*domain.EdgeResolvedConfig, error) {
	active, err := s.store.ReadActiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	p := active.Snapshot
	if subtle.ConstantTimeCompare([]byte(p.KeyConfirmation), []byte(s.protector.KeyHash(p.HubID))) != 1 {
		return nil, domain.ErrValidation
	}
	document, err := s.protector.Open(ctx, p.ProbeConfigMetadata, p.ProtectedPayload)
	if err != nil {
		return nil, err
	}
	defer clear(document)
	resolved, err := s.decoder.DecodeEdge(ctx, document, p.ProbeConfigTarget)
	if err != nil {
		return nil, err
	}
	if !domain.SameProbeConfigMetadata(resolved.Metadata, p.ProbeConfigMetadata) {
		return nil, domain.ErrValidation
	}
	return resolved, nil
}
