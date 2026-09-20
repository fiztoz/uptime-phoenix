package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type refFakeSourceRepo struct {
	source *domain.LocalProbeConfigSource
	err    error
}

func (r *refFakeSourceRepo) ReadLocal(ctx context.Context) (*domain.LocalProbeConfigSource, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.source, nil
}

type refFakeEncoder struct {
	data []byte
	err  error
}

func (e *refFakeEncoder) EncodeLocal(def domain.LocalProbeConfigDefinition) ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	if len(e.data) > 0 {
		return e.data, nil
	}
	return []byte(fmt.Sprintf("encoded-rev-%d", def.Revision)), nil
}

type refFakeInstallationRepo struct {
	inst *domain.ProbeInstallation
	err  error
}

func (r *refFakeInstallationRepo) Get(ctx context.Context) (*domain.ProbeInstallation, error) {
	if r.err != nil {
		return nil, r.err
	}
	if r.inst == nil {
		return nil, ports.ErrNotFound
	}
	return r.inst, nil
}

func (r *refFakeInstallationRepo) Initialize(ctx context.Context, inst domain.ProbeInstallation, verify func(domain.ProbeConfigMetadata, []byte) error) (*domain.ProbeInstallation, error) {
	r.inst = &inst
	return &inst, nil
}

func (r *refFakeInstallationRepo) HasSnapshots(ctx context.Context) (bool, error) {
	return false, nil
}

func (r *refFakeInstallationRepo) VerifyRetainedSnapshots(ctx context.Context, hubID string, check func(metadata domain.ProbeConfigMetadata, payload []byte) error) error {
	return nil
}

type refFakeAppliedReaderActivationRepo struct {
	*actFakeActivationRepo
	applied *domain.LocalProbeConfigDefinition
	readErr error
}

func (r *refFakeAppliedReaderActivationRepo) ReadAppliedLocal(ctx context.Context) (*domain.LocalProbeConfigDefinition, error) {
	if r.readErr != nil {
		return nil, r.readErr
	}
	return r.applied, nil
}

func TestLocalProbeConfigRefreshService_FirstActivationAndRefresh(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"

	source := &domain.LocalProbeConfigSource{
		Probe: domain.Probe{
			ID:      domain.LocalProbeID,
			Name:    "Local Probe",
			Kind:    domain.ProbeKindLocal,
			Enabled: true,
		},
		Assignments: []domain.ProbeConfigAssignment{},
	}
	sourceRepo := &refFakeSourceRepo{source: source}
	encoder := &refFakeEncoder{}

	configRepo := &actFakeConfigRepo{}
	protector := actFakeProtector{}
	fixedTime := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	inspector := &actFakeInspector{
		fn: func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
			hash := sha256.Sum256(document)
			return domain.ProbeConfigMetadata{
				ProbeConfigTarget: target,
				Revision:          1,
				SchemaVersion:     1,
				SHA256:            hex.EncodeToString(hash[:]),
				CreatedAt:         fixedTime,
				EffectiveAt:       fixedTime,
			}, nil
		},
	}
	preparedSvc := NewProbeConfigService(configRepo, inspector, protector)
	validationSvc := NewLocalProbeConfigValidationService(preparedSvc, actFakeValidator{})

	activationRepo := &refFakeAppliedReaderActivationRepo{
		actFakeActivationRepo: newActFakeActivationRepo(),
	}
	activationSvc := NewLocalProbeConfigActivationService(validationSvc, activationRepo)
	instRepo := &refFakeInstallationRepo{
		inst: &domain.ProbeInstallation{
			HubID:   hubID,
			KeyHash: strings.Repeat("k", 64),
		},
	}

	refreshSvc := NewLocalProbeConfigRefreshService(
		sourceRepo, encoder, preparedSvc, activationSvc, activationRepo, instRepo,
	)

	// 1. First activation: creates revision 1.
	active, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("first activation failed: %v", err)
	}
	if active.Revision != 1 {
		t.Fatalf("active revision = %d; want 1", active.Revision)
	}

	// 2. Second call when source is unchanged: returns same revision 1 without re-preparing.
	activationRepo.applied = &domain.LocalProbeConfigDefinition{Revision: 1}
	active2, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("second refresh failed: %v", err)
	}
	if active2.Revision != 1 {
		t.Fatalf("active2 revision = %d; want 1", active2.Revision)
	}

	// 3. Source changes (ReadAppliedLocal returns ErrConflict): prepares revision 2.
	activationRepo.readErr = ports.ErrConflict
	inspector.fn = func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
		hash := sha256.Sum256(document)
		return domain.ProbeConfigMetadata{
			ProbeConfigTarget: target,
			Revision:          2,
			SchemaVersion:     1,
			SHA256:            hex.EncodeToString(hash[:]),
			CreatedAt:         fixedTime,
			EffectiveAt:       fixedTime,
		}, nil
	}
	active3, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh on conflict failed: %v", err)
	}
	if active3.Revision != 2 {
		t.Fatalf("active3 revision = %d (expected 2); active3=%+v", active3.Revision, active3)
	}
}

func TestLocalProbeConfigRefreshService_ValidationFailure(t *testing.T) {
	ctx := context.Background()
	refreshSvc := NewLocalProbeConfigRefreshService(nil, nil, nil, nil, nil, nil)
	_, err := refreshSvc.Refresh(ctx)
	if err != domain.ErrValidation {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
}

func TestLocalProbeConfigRefreshService_UnactivatedPreparedRecovery(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"

	source := &domain.LocalProbeConfigSource{
		Probe: domain.Probe{
			ID:      domain.LocalProbeID,
			Name:    "Local Probe",
			Kind:    domain.ProbeKindLocal,
			Enabled: true,
		},
		Assignments: []domain.ProbeConfigAssignment{},
	}
	sourceRepo := &refFakeSourceRepo{source: source}
	encoder := &refFakeEncoder{}

	configRepo := &actFakeConfigRepo{}
	protector := actFakeProtector{}
	fixedTime := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	inspector := &actFakeInspector{
		fn: func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
			hash := sha256.Sum256(document)
			return domain.ProbeConfigMetadata{
				ProbeConfigTarget: target,
				Revision:          1,
				SchemaVersion:     1,
				SHA256:            hex.EncodeToString(hash[:]),
				CreatedAt:         fixedTime,
				EffectiveAt:       fixedTime,
			}, nil
		},
	}
	preparedSvc := NewProbeConfigService(configRepo, inspector, protector)
	validationSvc := NewLocalProbeConfigValidationService(preparedSvc, actFakeValidator{})

	activationRepo := &refFakeAppliedReaderActivationRepo{
		actFakeActivationRepo: newActFakeActivationRepo(),
	}
	activationSvc := NewLocalProbeConfigActivationService(validationSvc, activationRepo)
	instRepo := &refFakeInstallationRepo{
		inst: &domain.ProbeInstallation{
			HubID:   hubID,
			KeyHash: strings.Repeat("k", 64),
		},
	}

	// Prepare revision 1 beforehand (simulating crash before activation).
	builder := NewLocalProbeConfigBuilder(sourceRepo, encoder, preparedSvc)
	meta, err := builder.Prepare(ctx, hubID, 0, fixedTime, fixedTime)
	if err != nil {
		t.Fatalf("prepare failed: %v", err)
	}
	if meta.Revision != 1 {
		t.Fatalf("prepared revision = %d, want 1", meta.Revision)
	}

	refreshSvc := NewLocalProbeConfigRefreshService(
		sourceRepo, encoder, preparedSvc, activationSvc, activationRepo, instRepo,
	)

	// Refresh should activate revision 1 without preparing a duplicate.
	active, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh failed: %v", err)
	}
	if active.Revision != 1 {
		t.Fatalf("active revision = %d, want 1", active.Revision)
	}
}

func TestLocalProbeConfigRefreshService_UnactivatedPreparedSourceChanged(t *testing.T) {
	ctx := context.Background()
	hubID := "11111111-2222-4333-8444-555555555555"

	source := &domain.LocalProbeConfigSource{
		Probe: domain.Probe{
			ID:      domain.LocalProbeID,
			Name:    "Local Probe",
			Kind:    domain.ProbeKindLocal,
			Enabled: true,
		},
		Assignments: []domain.ProbeConfigAssignment{},
	}
	sourceRepo := &refFakeSourceRepo{source: source}
	encoder := &refFakeEncoder{data: []byte("initial-config")}

	configRepo := &actFakeConfigRepo{}
	protector := actFakeProtector{}
	fixedTime := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	inspector := &actFakeInspector{
		fn: func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
			hash := sha256.Sum256(document)
			return domain.ProbeConfigMetadata{
				ProbeConfigTarget: target,
				Revision:          1,
				SchemaVersion:     1,
				SHA256:            hex.EncodeToString(hash[:]),
				CreatedAt:         fixedTime,
				EffectiveAt:       fixedTime,
			}, nil
		},
	}
	preparedSvc := NewProbeConfigService(configRepo, inspector, protector)
	validationSvc := NewLocalProbeConfigValidationService(preparedSvc, actFakeValidator{})

	activationRepo := &refFakeAppliedReaderActivationRepo{
		actFakeActivationRepo: newActFakeActivationRepo(),
	}
	activationSvc := NewLocalProbeConfigActivationService(validationSvc, activationRepo)
	instRepo := &refFakeInstallationRepo{
		inst: &domain.ProbeInstallation{
			HubID:   hubID,
			KeyHash: strings.Repeat("k", 64),
		},
	}

	// Prepare revision 1.
	builder := NewLocalProbeConfigBuilder(sourceRepo, encoder, preparedSvc)
	_, err := builder.Prepare(ctx, hubID, 0, fixedTime, fixedTime)
	if err != nil {
		t.Fatalf("prepare rev 1 failed: %v", err)
	}

	// Now source changes before activation.
	encoder.data = []byte("changed-config")
	changedHash := sha256.Sum256([]byte("changed-config"))
	activationRepo.currentSourceHash = hex.EncodeToString(changedHash[:])
	inspector.fn = func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
		hash := sha256.Sum256(document)
		rev := int64(1)
		if string(document) == "changed-config" {
			rev = 2
		}
		return domain.ProbeConfigMetadata{
			ProbeConfigTarget: target,
			Revision:          rev,
			SchemaVersion:     1,
			SHA256:            hex.EncodeToString(hash[:]),
			CreatedAt:         fixedTime,
			EffectiveAt:       fixedTime,
		}, nil
	}

	refreshSvc := NewLocalProbeConfigRefreshService(
		sourceRepo, encoder, preparedSvc, activationSvc, activationRepo, instRepo,
	)

	// Refresh should detect that rev 1 hash doesn't match current source,
	// prepare rev 2 with expectedPreparedRevision=1, and activate rev 2!
	active, err := refreshSvc.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh after source change failed: %v", err)
	}
	if active.Revision != 2 {
		t.Fatalf("active revision = %d, want 2", active.Revision)
	}
}
