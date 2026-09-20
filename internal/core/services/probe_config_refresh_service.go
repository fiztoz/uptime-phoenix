package services

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeConfigRefreshService coordinates the preparation and atomic activation
// of local configuration snapshots when source data changes.
type LocalProbeConfigRefreshService struct {
	source           ports.LocalProbeConfigSourceRepository
	encoder          ports.LocalProbeConfigEncoder
	prepared         *ProbeConfigService
	activationSvc    *LocalProbeConfigActivationService
	activationRepo   ports.ProbeConfigActivationRepository
	installationRepo ports.ProbeInstallationRepository
	mu               sync.Mutex
	now              func() time.Time
}

// NewLocalProbeConfigRefreshService constructs a new refresh service.
func NewLocalProbeConfigRefreshService(
	source ports.LocalProbeConfigSourceRepository,
	encoder ports.LocalProbeConfigEncoder,
	prepared *ProbeConfigService,
	activationSvc *LocalProbeConfigActivationService,
	activationRepo ports.ProbeConfigActivationRepository,
	installationRepo ports.ProbeInstallationRepository,
) *LocalProbeConfigRefreshService {
	return &LocalProbeConfigRefreshService{
		source:           source,
		encoder:          encoder,
		prepared:         prepared,
		activationSvc:    activationSvc,
		activationRepo:   activationRepo,
		installationRepo: installationRepo,
		now:              time.Now,
	}
}

var _ ports.LocalProbeConfigRefresher = (*LocalProbeConfigRefreshService)(nil)

// Refresh verifies current source freshness against the active configuration.
// If the active configuration is stale or absent, it prepares and activates the next revision.
func (s *LocalProbeConfigRefreshService) Refresh(ctx context.Context) (*domain.ProbeActiveConfig, error) {
	if s == nil || s.source == nil || s.encoder == nil || s.prepared == nil ||
		s.activationSvc == nil || s.activationRepo == nil || s.installationRepo == nil {
		return nil, domain.ErrValidation
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for attempt := 0; attempt < 3; attempt++ {
		inst, err := s.installationRepo.Get(ctx)
		if err != nil {
			return nil, err
		}
		if inst == nil || inst.HubID == "" {
			return nil, domain.ErrValidation
		}

		active, err := s.activationRepo.GetActive(ctx, domain.LocalProbeID)
		expectedActiveRevision := int64(0)
		if err == nil && active != nil {
			expectedActiveRevision = active.Revision
			if reader, ok := s.activationRepo.(ports.LocalAppliedConfigReader); ok {
				applied, readErr := reader.ReadAppliedLocal(ctx)
				if readErr == nil && applied != nil && applied.Revision == active.Revision {
					return active, nil
				}
			}
		} else if !errors.Is(err, ports.ErrNotFound) {
			return nil, err
		}

		target := domain.ProbeConfigTarget{HubID: inst.HubID, ProbeID: domain.LocalProbeID}
		latestMeta, latestErr := s.prepared.Latest(ctx, domain.LocalProbeID)
		expectedPreparedRevision := int64(0)
		if latestErr == nil {
			expectedPreparedRevision = latestMeta.Revision
			if latestMeta.Revision > expectedActiveRevision {
				applied, actErr := s.activationSvc.Activate(ctx, target, latestMeta.Revision, latestMeta.SHA256, expectedActiveRevision)
				if actErr == nil {
					return applied, nil
				}
			}
		} else if !errors.Is(latestErr, ports.ErrNotFound) {
			return nil, latestErr
		}

		builder := NewLocalProbeConfigBuilder(s.source, s.encoder, s.prepared)
		now := s.now().UTC()
		meta, err := builder.Prepare(ctx, inst.HubID, expectedPreparedRevision, now, now)
		if err != nil {
			if errors.Is(err, ports.ErrConflict) {
				continue
			}
			return nil, err
		}

		applied, err := s.activationSvc.Activate(ctx, meta.ProbeConfigTarget, meta.Revision, meta.SHA256, expectedActiveRevision)
		if err == nil {
			return applied, nil
		}
		if !errors.Is(err, ports.ErrConflict) {
			return nil, err
		}
		// If conflict occurred, another worker may have just activated. Loop will re-check.
	}

	active, err := s.activationRepo.GetActive(ctx, domain.LocalProbeID)
	if err == nil && active != nil {
		if reader, ok := s.activationRepo.(ports.LocalAppliedConfigReader); ok {
			applied, readErr := reader.ReadAppliedLocal(ctx)
			if readErr == nil && applied != nil && applied.Revision == active.Revision {
				return active, nil
			}
		}
	}
	return nil, ports.ErrConflict
}

// StartEventSubscription listens for events that modify the local configuration source
// and triggers a background refresh.
func (s *LocalProbeConfigRefreshService) StartEventSubscription(ctx context.Context, bus ports.EventBus) {
	if s == nil || bus == nil {
		return
	}
	events := []string{"monitor.update", "monitor.delete", "status.change", "config.refresh"}
	for _, eventType := range events {
		ch := bus.Subscribe(eventType)
		go func(c <-chan ports.Event) {
			for {
				select {
				case <-ctx.Done():
					return
				case _, ok := <-c:
					if !ok {
						return
					}
					_, _ = s.Refresh(ctx)
				}
			}
		}(ch)
	}
}
