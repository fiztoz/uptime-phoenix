package services

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeInstallationService manages startup verification and installation identity.
// It never activates configuration, schedules checks, or enables provider delivery.
type ProbeInstallationService struct {
	repo ports.ProbeInstallationRepository
}

// NewProbeInstallationService creates a new installation identity service.
func NewProbeInstallationService(repo ports.ProbeInstallationRepository) *ProbeInstallationService {
	return &ProbeInstallationService{repo: repo}
}

// InitializeOrVerify initializes a new installation identity or verifies an existing one
// against the configured key protector and optional explicit hub ID.
// It verifies that all retained configuration snapshots match the installation authority
// and can be successfully authenticated by the key protector.
func (s *ProbeInstallationService) InitializeOrVerify(ctx context.Context, protector ports.ProbeConfigProtector, configuredHubID string) (*domain.ProbeInstallation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.repo == nil || protector == nil {
		return nil, domain.ErrValidation
	}
	if configuredHubID != "" && !domain.ValidHubID(configuredHubID) {
		return nil, domain.ErrValidation
	}

	existing, err := s.repo.Get(ctx)
	if err == nil {
		if configuredHubID != "" && existing.HubID != configuredHubID {
			return nil, domain.ErrProbeInstallationConflict
		}
		expectedKeyHash := protector.KeyHash(existing.HubID)
		if expectedKeyHash == "" || existing.KeyHash != expectedKeyHash {
			return nil, domain.ErrProbeKeyMismatch
		}
		verErr := s.repo.VerifyRetainedSnapshots(ctx, existing.HubID, func(meta domain.ProbeConfigMetadata, payload []byte) error {
			if _, openErr := protector.Open(ctx, meta, payload); openErr != nil {
				return fmt.Errorf("retained snapshot verification failed: %w", domain.ErrValidation)
			}
			return nil
		})
		if verErr != nil {
			return nil, verErr
		}
		return existing, nil
	}

	if !errors.Is(err, ports.ErrNotFound) {
		return nil, fmt.Errorf("lookup probe installation: %w", err)
	}

	var hubID string
	if configuredHubID != "" {
		hubID = configuredHubID
		verErr := s.repo.VerifyRetainedSnapshots(ctx, hubID, func(meta domain.ProbeConfigMetadata, payload []byte) error {
			if _, openErr := protector.Open(ctx, meta, payload); openErr != nil {
				return fmt.Errorf("retained snapshot verification failed: %w", domain.ErrValidation)
			}
			return nil
		})
		if verErr != nil {
			return nil, verErr
		}
	} else {
		hasSnapshots, serr := s.repo.HasSnapshots(ctx)
		if serr != nil {
			return nil, fmt.Errorf("check retained snapshots: %w", serr)
		}
		if hasSnapshots {
			return nil, fmt.Errorf("existing prepared snapshots present without verified installation identity: %w", domain.ErrProbeInstallationConflict)
		}
		newID, gerr := newUUIDv4()
		if gerr != nil {
			return nil, fmt.Errorf("generate installation identity: %w", gerr)
		}
		hubID = newID
	}

	keyHash := protector.KeyHash(hubID)
	if keyHash == "" {
		return nil, domain.ErrValidation
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	inst := domain.ProbeInstallation{
		HubID:          hubID,
		KeyHash:        keyHash,
		ProtocolFloor:  1,
		AuthorityEpoch: 1,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	created, ierr := s.repo.Initialize(ctx, inst, func(meta domain.ProbeConfigMetadata, payload []byte) error {
		if _, err := protector.Open(ctx, meta, payload); err != nil {
			return domain.ErrProbeKeyMismatch
		}
		return nil
	})
	if ierr != nil {
		if errors.Is(ierr, ports.ErrConflict) {
			recheck, rerr := s.repo.Get(ctx)
			if rerr == nil {
				if configuredHubID != "" && recheck.HubID != configuredHubID {
					return nil, domain.ErrProbeInstallationConflict
				}
				if recheck.KeyHash != protector.KeyHash(recheck.HubID) {
					return nil, domain.ErrProbeKeyMismatch
				}
				return recheck, nil
			}
		}
		return nil, fmt.Errorf("initialize probe installation: %w", ierr)
	}
	return created, nil
}

func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
