package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type actFakeActivationRepo struct {
	active            map[string]*domain.ProbeActiveConfig
	receipts          map[string]map[int64]*domain.ProbeActiveConfig
	currentSourceHash string
	err               error
}

func newActFakeActivationRepo() *actFakeActivationRepo {
	return &actFakeActivationRepo{
		active:   make(map[string]*domain.ProbeActiveConfig),
		receipts: make(map[string]map[int64]*domain.ProbeActiveConfig),
	}
}

func (f *actFakeActivationRepo) GetActive(ctx context.Context, probeID string) (*domain.ProbeActiveConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	cfg, ok := f.active[probeID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return cfg, nil
}

func (f *actFakeActivationRepo) GetReceipt(ctx context.Context, probeID string, revision int64) (*domain.ProbeActiveConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	byProbe, ok := f.receipts[probeID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cfg, ok := byProbe[revision]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return cfg, nil
}

func (f *actFakeActivationRepo) ActivateLocal(ctx context.Context, params ports.LocalActivationParams) (*domain.ProbeActiveConfig, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.currentSourceHash != "" && params.SHA256 != f.currentSourceHash {
		return nil, ports.ErrConflict
	}
	if active, ok := f.active[params.Target.ProbeID]; ok {
		if active.Revision != params.ExpectedActiveRevision || params.Revision <= active.Revision {
			return nil, ports.ErrConflict
		}
	} else if params.ExpectedActiveRevision != 0 {
		return nil, ports.ErrConflict
	}
	result := &domain.ProbeActiveConfig{
		ProbeConfigTarget: params.Target,
		Revision:          params.Revision,
		SHA256:            params.SHA256,
		AppliedAt:         params.AppliedAt.UTC(),
		AssignmentCount:   params.AssignmentCount,
	}
	f.active[params.Target.ProbeID] = result
	if f.receipts[params.Target.ProbeID] == nil {
		f.receipts[params.Target.ProbeID] = make(map[int64]*domain.ProbeActiveConfig)
	}
	f.receipts[params.Target.ProbeID][params.Revision] = result
	return result, nil
}

type actFakeConfigRepo struct {
	snapshots map[int64]*domain.ProtectedProbeConfig
}

func (f *actFakeConfigRepo) Save(ctx context.Context, snapshot domain.ProtectedProbeConfig, expectedRevision int64) (*domain.ProtectedProbeConfig, error) {
	if f.snapshots == nil {
		f.snapshots = make(map[int64]*domain.ProtectedProbeConfig)
	}
	var maxRev int64
	var latest *domain.ProtectedProbeConfig
	for rev, s := range f.snapshots {
		if rev > maxRev {
			maxRev = rev
			latest = s
		}
	}
	if latest != nil {
		if latest.Revision == snapshot.Revision {
			if domain.SameProbeConfigMetadata(latest.ProbeConfigMetadata, snapshot.ProbeConfigMetadata) {
				return latest, nil
			}
			return nil, ports.ErrConflict
		}
		if latest.Revision != expectedRevision || snapshot.Revision <= latest.Revision {
			return nil, ports.ErrConflict
		}
	} else if expectedRevision != 0 {
		return nil, ports.ErrConflict
	}
	f.snapshots[snapshot.Revision] = &snapshot
	return &snapshot, nil
}

func (f *actFakeConfigRepo) Get(ctx context.Context, probeID string, revision int64) (*domain.ProtectedProbeConfig, error) {
	s, ok := f.snapshots[revision]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return s, nil
}

func (f *actFakeConfigRepo) Latest(ctx context.Context, probeID string) (*domain.ProtectedProbeConfig, error) {
	var maxRev int64
	var latest *domain.ProtectedProbeConfig
	for rev, s := range f.snapshots {
		if rev > maxRev {
			maxRev = rev
			latest = s
		}
	}
	if latest == nil {
		return nil, ports.ErrNotFound
	}
	return latest, nil
}

type actFakeProtector struct{}

func (actFakeProtector) Seal(ctx context.Context, metadata domain.ProbeConfigMetadata, plaintext []byte) ([]byte, error) {
	return append([]byte{1}, plaintext...), nil
}

func (actFakeProtector) Open(ctx context.Context, metadata domain.ProbeConfigMetadata, ciphertext []byte) ([]byte, error) {
	if len(ciphertext) <= 1 {
		return nil, domain.ErrValidation
	}
	return ciphertext[1:], nil
}

func (actFakeProtector) KeyHash(hubID string) string {
	return strings.Repeat("k", 64)
}

type actFakeInspector struct {
	fn func(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error)
}

func (f actFakeInspector) Inspect(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
	if f.fn != nil {
		return f.fn(document, target)
	}
	return domain.ProbeConfigMetadata{}, domain.ErrValidation
}

type actFakeValidator struct {
	err error
}

func (f actFakeValidator) ValidateLocal(ctx context.Context, document []byte, target domain.ProbeConfigTarget) error {
	return f.err
}

func TestLocalProbeConfigActivationService_Validation(t *testing.T) {
	ctx := context.Background()
	validTarget := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: domain.LocalProbeID}
	validHash := strings.Repeat("a", 64)

	repo := newActFakeActivationRepo()
	prepSvc := NewProbeConfigService(&actFakeConfigRepo{}, actFakeInspector{}, actFakeProtector{})
	valSvc := NewLocalProbeConfigValidationService(prepSvc, actFakeValidator{})
	svc := NewLocalProbeConfigActivationService(valSvc, repo)

	t.Run("invalid target", func(t *testing.T) {
		badTargets := []domain.ProbeConfigTarget{
			{HubID: "not-a-uuid", ProbeID: domain.LocalProbeID},
			{HubID: validTarget.HubID, ProbeID: "remote-probe"},
			{HubID: validTarget.HubID, ProbeID: "22222222-2222-4222-8222-222222222222"},
		}
		for _, target := range badTargets {
			if _, err := svc.Activate(ctx, target, 1, validHash, 0); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected ErrValidation for target %+v, got %v", target, err)
			}
		}
	})

	t.Run("invalid revision", func(t *testing.T) {
		for _, rev := range []int64{0, -1, -99} {
			if _, err := svc.Activate(ctx, validTarget, rev, validHash, 0); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected ErrValidation for revision %d, got %v", rev, err)
			}
		}
	})

	t.Run("invalid hash", func(t *testing.T) {
		for _, hash := range []string{"", "short", strings.Repeat("z", 64), strings.Repeat("A", 64)} {
			if _, err := svc.Activate(ctx, validTarget, 1, hash, 0); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("expected ErrValidation for hash %q, got %v", hash, err)
			}
		}
	})

	t.Run("negative expectedActiveRevision", func(t *testing.T) {
		if _, err := svc.Activate(ctx, validTarget, 1, validHash, -1); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("expected ErrValidation for negative expectedActiveRevision, got %v", err)
		}
	})

	t.Run("context canceled", func(t *testing.T) {
		cancCtx, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := svc.Activate(cancCtx, validTarget, 1, validHash, 0); !errors.Is(err, context.Canceled) {
			t.Fatalf("expected Canceled, got %v", err)
		}
	})
}

func TestLocalProbeConfigActivationService_Activate(t *testing.T) {
	ctx := context.Background()
	validTarget := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: domain.LocalProbeID}
	validHash := strings.Repeat("a", 64)

	t.Run("validation failure propagates without activating", func(t *testing.T) {
		repo := newActFakeActivationRepo()
		configRepo := &actFakeConfigRepo{} // empty, so Read will return ErrNotFound
		prepSvc := NewProbeConfigService(configRepo, actFakeInspector{}, actFakeProtector{})
		valSvc := NewLocalProbeConfigValidationService(prepSvc, actFakeValidator{})
		svc := NewLocalProbeConfigActivationService(valSvc, repo)

		_, err := svc.Activate(ctx, validTarget, 1, validHash, 0)
		if !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("expected ErrNotFound, got %v", err)
		}
		if len(repo.active) != 0 {
			t.Fatalf("expected no activation, but found %d", len(repo.active))
		}
	})

	t.Run("hash mismatch conflicts", func(t *testing.T) {
		repo := newActFakeActivationRepo()
		configRepo := &actFakeConfigRepo{}
		now := time.Now().UTC().Truncate(time.Microsecond)
		doc := []byte("test document bytes")
		hash := sha256.Sum256(doc)
		realHash := hex.EncodeToString(hash[:])
		meta := domain.ProbeConfigMetadata{
			ProbeConfigTarget: validTarget,
			Revision:          1,
			SchemaVersion:     1,
			SHA256:            realHash,
			CreatedAt:         now,
			EffectiveAt:       now,
		}
		_, _ = configRepo.Save(ctx, domain.ProtectedProbeConfig{
			ProbeConfigMetadata: meta,
			ProtectedPayload:    append([]byte{1}, doc...),
			StoredAt:            now,
		}, 0)

		inspector := actFakeInspector{fn: func(doc []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
			return meta, nil
		}}
		prepSvc := NewProbeConfigService(configRepo, inspector, actFakeProtector{})
		valSvc := NewLocalProbeConfigValidationService(prepSvc, actFakeValidator{})
		svc := NewLocalProbeConfigActivationService(valSvc, repo)

		// Expected hash is "a...", but stored is realHash
		_, err := svc.Activate(ctx, validTarget, 1, validHash, 0)
		if !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("expected ErrConflict on hash mismatch, got %v", err)
		}
		if len(repo.active) != 0 {
			t.Fatalf("expected no activation, but found %d", len(repo.active))
		}
	})

	t.Run("successful activation", func(t *testing.T) {
		repo := newActFakeActivationRepo()
		configRepo := &actFakeConfigRepo{}
		now := time.Now().UTC().Truncate(time.Microsecond)
		doc := []byte("test document bytes")
		hash := sha256.Sum256(doc)
		realHash := hex.EncodeToString(hash[:])
		meta := domain.ProbeConfigMetadata{
			ProbeConfigTarget: validTarget,
			Revision:          1,
			SchemaVersion:     1,
			SHA256:            realHash,
			CreatedAt:         now,
			EffectiveAt:       now,
		}
		_, _ = configRepo.Save(ctx, domain.ProtectedProbeConfig{
			ProbeConfigMetadata: meta,
			ProtectedPayload:    append([]byte{1}, doc...),
			StoredAt:            now,
		}, 0)

		inspector := actFakeInspector{fn: func(doc []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
			return meta, nil
		}}
		prepSvc := NewProbeConfigService(configRepo, inspector, actFakeProtector{})
		valSvc := NewLocalProbeConfigValidationService(prepSvc, actFakeValidator{})
		svc := NewLocalProbeConfigActivationService(valSvc, repo)

		res, err := svc.Activate(ctx, validTarget, 1, realHash, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Revision != 1 || res.SHA256 != realHash || res.ProbeID != domain.LocalProbeID {
			t.Fatalf("unexpected activation result: %+v", res)
		}

		// Verify GetActive
		active, err := svc.GetActive(ctx, domain.LocalProbeID)
		if err != nil || active.Revision != 1 {
			t.Fatalf("GetActive failed: %v, %+v", err, active)
		}

		// Verify GetReceipt
		receipt, err := svc.GetReceipt(ctx, domain.LocalProbeID, 1)
		if err != nil || receipt.Revision != 1 {
			t.Fatalf("GetReceipt failed: %v, %+v", err, receipt)
		}
	})
}
