package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeConfigService prepares and retrieves protected complete configuration.
// It is an internal confidential boundary, not an execution/activation service.
type ProbeConfigService struct {
	repo      ports.ProbeConfigRepository
	inspector ports.ProbeConfigInspector
	protector ports.ProbeConfigProtector
}

// NewProbeConfigService requires explicit storage, schema inspection and protection.
func NewProbeConfigService(repo ports.ProbeConfigRepository, inspector ports.ProbeConfigInspector, protector ports.ProbeConfigProtector) *ProbeConfigService {
	return &ProbeConfigService{repo: repo, inspector: inspector, protector: protector}
}

// Prepare protects exact document bytes before any write. The result is metadata
// only, never a config.applied receipt. Callers must authorize the trusted target.
func (s *ProbeConfigService) Prepare(ctx context.Context, target domain.ProbeConfigTarget, document []byte, expectedRevision int64) (domain.ProbeConfigMetadata, error) {
	if err := s.ready(ctx, target); err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	if len(document) == 0 || len(document) > domain.MaxProbeConfigBytes || expectedRevision < 0 {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	document = bytes.Clone(document)
	metadata, err := s.inspect(document, target)
	if err != nil {
		return domain.ProbeConfigMetadata{}, err
	}
	payload, err := s.protector.Seal(ctx, metadata, document)
	if err != nil {
		return domain.ProbeConfigMetadata{}, fmt.Errorf("protect prepared configuration: %w", err)
	}
	stored, err := s.repo.Save(ctx, domain.ProtectedProbeConfig{ProbeConfigMetadata: metadata, ProtectedPayload: payload, KeyConfirmation: s.protector.KeyHash(target.HubID)}, expectedRevision)
	if err != nil {
		return domain.ProbeConfigMetadata{}, fmt.Errorf("store prepared configuration: %w", err)
	}
	return stored.ProbeConfigMetadata, nil
}

// Read returns confidential original bytes from an exact revision, or the latest
// prepared revision when revision is zero. No fallback to older credentials is
// performed. Runtime activation and current-channel reconciliation remain required.
func (s *ProbeConfigService) Read(ctx context.Context, target domain.ProbeConfigTarget, revision int64) ([]byte, domain.ProbeConfigMetadata, error) {
	if err := s.ready(ctx, target); err != nil {
		return nil, domain.ProbeConfigMetadata{}, err
	}
	if revision < 0 {
		return nil, domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	var stored *domain.ProtectedProbeConfig
	var err error
	if revision == 0 {
		stored, err = s.repo.Latest(ctx, target.ProbeID)
	} else {
		stored, err = s.repo.Get(ctx, target.ProbeID, revision)
	}
	if err != nil {
		return nil, domain.ProbeConfigMetadata{}, fmt.Errorf("read prepared configuration: %w", err)
	}
	if stored.ProbeConfigTarget != target || revision != 0 && stored.Revision != revision {
		return nil, domain.ProbeConfigMetadata{}, ports.ErrConflict
	}
	plaintext, err := s.protector.Open(ctx, stored.ProbeConfigMetadata, stored.ProtectedPayload)
	if err != nil {
		return nil, domain.ProbeConfigMetadata{}, fmt.Errorf("open prepared configuration: %w", err)
	}
	metadata, err := s.inspect(plaintext, target)
	if err != nil || !domain.SameProbeConfigMetadata(metadata, stored.ProbeConfigMetadata) {
		return nil, domain.ProbeConfigMetadata{}, fmt.Errorf("prepared configuration integrity: %w", domain.ErrValidation)
	}
	return plaintext, metadata, nil
}

func (s *ProbeConfigService) inspect(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
	if len(document) == 0 || len(document) > domain.MaxProbeConfigBytes {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	metadata, err := s.inspector.Inspect(document, target)
	// Do not relay secret-bearing decoder/validator errors across this boundary.
	if err != nil || !domain.ValidProbeConfigMetadata(metadata) || metadata.ProbeConfigTarget != target {
		return domain.ProbeConfigMetadata{}, fmt.Errorf("invalid configuration document: %w", domain.ErrValidation)
	}
	hash := sha256.Sum256(document)
	if metadata.SHA256 != hex.EncodeToString(hash[:]) {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	return domain.NormalizeProbeConfigMetadata(metadata), nil
}

func (s *ProbeConfigService) ready(ctx context.Context, target domain.ProbeConfigTarget) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s == nil || s.repo == nil || s.inspector == nil || s.protector == nil || !domain.ValidProbeConfigTarget(target) {
		return fmt.Errorf("configuration preparation unavailable: %w", domain.ErrValidation)
	}
	return nil
}
