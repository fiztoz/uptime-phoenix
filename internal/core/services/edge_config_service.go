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
	applications, err := edgeRegionalApplications(resolved, i.StreamID)
	if err != nil {
		return nil, err
	}
	command := domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{ProbeConfigMetadata: resolved.Metadata, KeyConfirmation: s.protector.KeyHash(i.HubID), ProtectedPayload: protected}, Assignments: indices, AppliedAt: at.UTC(), ConnectionGeneration: generation, RegionalDeliveryApplications: applications}
	if err := s.store.ActivateConfig(ctx, command); err != nil {
		return nil, err
	}
	return resolved, nil
}

// ReadRegionalApplications returns durable selected-stream regional facts for
// the exact retained document. It authenticates the captured document and facts
// never synthesizes evidence from desired assignments or mutable source rows.
func (s *EdgeConfigService) ReadRegionalApplications(ctx context.Context, metadata domain.ProbeConfigMetadata, generation int64, streamID string) ([]domain.ProbeDeliveryModeApplication, error) {
	if generation <= 0 || !domain.ValidProbeConfigMetadata(metadata) || !domain.ValidHubID(streamID) || streamID == domain.LocalStreamID {
		return nil, domain.ErrValidation
	}
	identity, err := s.identity.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if metadata.ProbeConfigTarget != (domain.ProbeConfigTarget{HubID: identity.HubID, ProbeID: identity.ProbeID}) || identity.ConfigRevision != metadata.Revision || identity.ConnectionGeneration != generation || identity.StreamID != streamID {
		return nil, ports.ErrConflict
	}
	active, err := s.store.ReadActiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	if !domain.SameProbeConfigMetadata(active.Snapshot.ProbeConfigMetadata, metadata) {
		return nil, ports.ErrConflict
	}
	if _, err := s.decodeActive(ctx, active); err != nil {
		return nil, err
	}
	for _, fact := range active.RegionalDeliveryApplications {
		if fact.StreamID != streamID {
			return nil, ports.ErrConflict
		}
	}
	current, err := s.identity.ReadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if current.HubID != identity.HubID || current.ProbeID != identity.ProbeID || current.ConfigRevision != metadata.Revision || current.ConnectionGeneration != generation || current.StreamID != streamID {
		return nil, ports.ErrConflict
	}
	return append([]domain.ProbeDeliveryModeApplication(nil), active.RegionalDeliveryApplications...), nil
}

// Load authenticates exact active bytes and reruns the current build's validators.
// Wrong keys, unsupported settings and corrupt bytes never fall back to old config.
func (s *EdgeConfigService) Load(ctx context.Context) (*domain.EdgeResolvedConfig, error) {
	active, err := s.store.ReadActiveConfig(ctx)
	if err != nil {
		return nil, err
	}
	return s.decodeActive(ctx, active)
}

// decodeActive validates one captured snapshot/fact tuple. A second storage read
// would authenticate different facts during concurrent activation or repair.
func (s *EdgeConfigService) decodeActive(ctx context.Context, active domain.EdgeActiveConfig) (*domain.EdgeResolvedConfig, error) {
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
	if resolved == nil || !domain.SameProbeConfigMetadata(resolved.Metadata, p.ProbeConfigMetadata) {
		return nil, domain.ErrValidation
	}
	if err := validateEdgeRegionalApplications(resolved, active.RegionalDeliveryApplications); err != nil {
		return nil, err
	}
	return resolved, nil
}
