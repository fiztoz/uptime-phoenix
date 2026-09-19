package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type mockProbeInstallationRepo struct {
	installation *domain.ProbeInstallation
	getErr       error
	initErr      error
	hasSnapshots bool
	snapshots    []struct {
		meta    domain.ProbeConfigMetadata
		payload []byte
	}
}

func (m *mockProbeInstallationRepo) Get(ctx context.Context) (*domain.ProbeInstallation, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	if m.installation == nil {
		return nil, ports.ErrNotFound
	}
	copy := *m.installation
	return &copy, nil
}

func (m *mockProbeInstallationRepo) Initialize(ctx context.Context, inst domain.ProbeInstallation) (*domain.ProbeInstallation, error) {
	if m.initErr != nil {
		return nil, m.initErr
	}
	if m.installation != nil {
		if domain.SameProbeInstallation(*m.installation, inst) {
			copy := *m.installation
			return &copy, nil
		}
		return nil, ports.ErrConflict
	}
	copy := inst
	m.installation = &copy
	return &copy, nil
}

func (m *mockProbeInstallationRepo) HasSnapshots(ctx context.Context) (bool, error) {
	return m.hasSnapshots || len(m.snapshots) > 0, nil
}

func (m *mockProbeInstallationRepo) VerifyRetainedSnapshots(ctx context.Context, hubID string, check func(metadata domain.ProbeConfigMetadata, payload []byte) error) error {
	for _, s := range m.snapshots {
		if s.meta.HubID != hubID {
			return domain.ErrProbeInstallationConflict
		}
		if err := check(s.meta, s.payload); err != nil {
			return err
		}
	}
	return nil
}

type mockProbeProtector struct {
	keyHashPrefix string
	openErr       error
}

func (p *mockProbeProtector) Seal(ctx context.Context, metadata domain.ProbeConfigMetadata, plaintext []byte) ([]byte, error) {
	return append([]byte{1}, plaintext...), nil
}

func (p *mockProbeProtector) Open(ctx context.Context, metadata domain.ProbeConfigMetadata, ciphertext []byte) ([]byte, error) {
	if p.openErr != nil {
		return nil, p.openErr
	}
	if len(ciphertext) <= 1 {
		return nil, domain.ErrValidation
	}
	return ciphertext[1:], nil
}

func (p *mockProbeProtector) KeyHash(hubID string) string {
	if !domain.ValidHubID(hubID) {
		return ""
	}
	// Return a deterministic 64-char hex string
	prefix := p.keyHashPrefix
	if prefix == "" {
		prefix = "11111111"
	}
	return fmt.Sprintf("%-64s", prefix)[:64]
}

func TestProbeInstallationService_EmptyDB_NoSnapshots(t *testing.T) {
	ctx := context.Background()
	repo := &mockProbeInstallationRepo{}
	protector := &mockProbeProtector{keyHashPrefix: "aaaaaaaa"}
	svc := NewProbeInstallationService(repo)

	// 1. Without configured hub ID -> generates fresh UUID
	inst, err := svc.InitializeOrVerify(ctx, protector, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !domain.ValidHubID(inst.HubID) {
		t.Fatalf("expected valid generated hub ID, got %q", inst.HubID)
	}
	if inst.KeyHash != protector.KeyHash(inst.HubID) {
		t.Fatalf("expected key hash %q, got %q", protector.KeyHash(inst.HubID), inst.KeyHash)
	}
	if repo.installation == nil || repo.installation.HubID != inst.HubID {
		t.Fatalf("installation was not saved to repo")
	}

	// 2. Calling again with the same protector -> idempotent success
	inst2, err := svc.InitializeOrVerify(ctx, protector, inst.HubID)
	if err != nil {
		t.Fatalf("unexpected error on idempotent call: %v", err)
	}
	if !domain.SameProbeInstallation(*inst, *inst2) {
		t.Fatalf("expected identical installation records")
	}
}

func TestProbeInstallationService_EmptyDB_WithConfiguredHubID(t *testing.T) {
	ctx := context.Background()
	repo := &mockProbeInstallationRepo{}
	protector := &mockProbeProtector{keyHashPrefix: "bbbbbbbb"}
	svc := NewProbeInstallationService(repo)

	configuredID := "11111111-2222-4333-8444-555555555555"
	inst, err := svc.InitializeOrVerify(ctx, protector, configuredID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inst.HubID != configuredID {
		t.Fatalf("expected hub ID %q, got %q", configuredID, inst.HubID)
	}
}

func TestProbeInstallationService_EmptyDB_HasSnapshotsWithoutConfiguredHubID(t *testing.T) {
	ctx := context.Background()
	repo := &mockProbeInstallationRepo{hasSnapshots: true}
	protector := &mockProbeProtector{keyHashPrefix: "cccccccc"}
	svc := NewProbeInstallationService(repo)

	// Must refuse to silently adopt a snapshot's hub ID without explicit configuration
	_, err := svc.InitializeOrVerify(ctx, protector, "")
	if !errors.Is(err, domain.ErrProbeInstallationConflict) {
		t.Fatalf("expected ErrProbeInstallationConflict, got %v", err)
	}
}

func TestProbeInstallationService_ExistingInstallation_KeyMismatch(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	protectorA := &mockProbeProtector{keyHashPrefix: "aaaaaaaa"}
	protectorB := &mockProbeProtector{keyHashPrefix: "bbbbbbbb"}

	repo := &mockProbeInstallationRepo{
		installation: &domain.ProbeInstallation{
			HubID:          hubID,
			KeyHash:        protectorA.KeyHash(hubID),
			ProtocolFloor:  1,
			AuthorityEpoch: 1,
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		},
	}
	svc := NewProbeInstallationService(repo)

	// Using protectorB with different key hash must fail with ErrProbeKeyMismatch
	_, err := svc.InitializeOrVerify(ctx, protectorB, "")
	if !errors.Is(err, domain.ErrProbeKeyMismatch) {
		t.Fatalf("expected ErrProbeKeyMismatch, got %v", err)
	}
}

func TestProbeInstallationService_ExistingInstallation_HubIDConflict(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	protector := &mockProbeProtector{keyHashPrefix: "aaaaaaaa"}

	repo := &mockProbeInstallationRepo{
		installation: &domain.ProbeInstallation{
			HubID:          hubID,
			KeyHash:        protector.KeyHash(hubID),
			ProtocolFloor:  1,
			AuthorityEpoch: 1,
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		},
	}
	svc := NewProbeInstallationService(repo)

	// Providing a different configured hub ID must fail with ErrProbeInstallationConflict
	_, err := svc.InitializeOrVerify(ctx, protector, "22222222-2222-4222-8222-222222222222")
	if !errors.Is(err, domain.ErrProbeInstallationConflict) {
		t.Fatalf("expected ErrProbeInstallationConflict, got %v", err)
	}
}

func TestProbeInstallationService_RetainedSnapshotDecryptionFailure(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"
	protector := &mockProbeProtector{
		keyHashPrefix: "aaaaaaaa",
		openErr:       fmt.Errorf("secret-fixture-corrupt: %w", domain.ErrValidation),
	}

	repo := &mockProbeInstallationRepo{
		installation: &domain.ProbeInstallation{
			HubID:          hubID,
			KeyHash:        protector.KeyHash(hubID),
			ProtocolFloor:  1,
			AuthorityEpoch: 1,
			CreatedAt:      time.Now().UTC(),
			UpdatedAt:      time.Now().UTC(),
		},
		snapshots: []struct {
			meta    domain.ProbeConfigMetadata
			payload []byte
		}{
			{
				meta: domain.ProbeConfigMetadata{
					ProbeConfigTarget: domain.ProbeConfigTarget{HubID: hubID, ProbeID: domain.LocalProbeID},
					Revision:          1,
				},
				payload: []byte{1, 2, 3},
			},
		},
	}
	svc := NewProbeInstallationService(repo)

	_, err := svc.InitializeOrVerify(ctx, protector, "")
	if err == nil {
		t.Fatalf("expected error when snapshot decryption fails")
	}
	if strings.Contains(err.Error(), "secret-fixture") {
		t.Fatalf("secret leaked in error message: %v", err)
	}
}

func TestProbeInstallationService_InputValidationAndCancellation(t *testing.T) {
	ctx := context.Background()
	protector := &mockProbeProtector{keyHashPrefix: "aaaaaaaa"}
	repo := &mockProbeInstallationRepo{}

	// Nil receiver
	var nilSvc *ProbeInstallationService
	if _, err := nilSvc.InitializeOrVerify(ctx, protector, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation on nil receiver, got %v", err)
	}

	// Nil protector
	svc := NewProbeInstallationService(repo)
	if _, err := svc.InitializeOrVerify(ctx, nil, ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation on nil protector, got %v", err)
	}

	// Invalid configured hub ID
	if _, err := svc.InitializeOrVerify(ctx, protector, "invalid-uuid"); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("expected ErrValidation on invalid hub ID, got %v", err)
	}

	// Canceled context
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := svc.InitializeOrVerify(canceledCtx, protector, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
