package services

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalProbeConfigRefreshService coordinates the preparation and atomic activation
// of local configuration snapshots when source data changes.
type LocalProbeConfigRefreshService struct {
	source            ports.LocalProbeConfigSourceRepository
	encoder           ports.LocalProbeConfigEncoder
	prepared          *ProbeConfigService
	activationSvc     *LocalProbeConfigActivationService
	activationRepo    ports.ProbeConfigActivationRepository
	installationRepo  ports.ProbeInstallationRepository
	localPreparations ports.LocalDeliveryPreparationReader
	mu                sync.Mutex
	now               func() time.Time
	reconcileInterval time.Duration
	startOnce         sync.Once
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
		source:            source,
		encoder:           encoder,
		prepared:          prepared,
		activationSvc:     activationSvc,
		activationRepo:    activationRepo,
		installationRepo:  installationRepo,
		now:               time.Now,
		reconcileInterval: 5 * time.Second,
	}
}

// SetDeliveryPreparations enables pending-request completeness for protected
// local refreshes. It never enables application or changes execution behavior.
// Configure before starting the worker.
func (s *LocalProbeConfigRefreshService) SetDeliveryPreparations(reader ports.LocalDeliveryPreparationReader) {
	s.localPreparations = reader
}

// SetReconcileInterval configures the periodic reconciliation check interval.
func (s *LocalProbeConfigRefreshService) SetReconcileInterval(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileInterval = d
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
				if readErr == nil && applied != nil && applied.Revision == active.Revision && localConfigHasAssignmentBindings(applied) && localConfigHasRegionalApplications(applied) {
					complete, prepErr := s.localPreparationsComplete(ctx, *active, applied)
					if prepErr != nil {
						return nil, prepErr
					}
					if complete {
						return active, nil
					}
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
			if readErr == nil && applied != nil && applied.Revision == active.Revision && localConfigHasAssignmentBindings(applied) && localConfigHasRegionalApplications(applied) {
				complete, prepErr := s.localPreparationsComplete(ctx, *active, applied)
				if prepErr != nil {
					return nil, prepErr
				}
				if complete {
					return active, nil
				}
			}
		}
	}
	return nil, ports.ErrConflict
}

// localConfigHasAssignmentBindings keeps legacy snapshots executable while
// allowing the existing refresher to publish a new bound revision on upgrade.
func localConfigHasAssignmentBindings(definition *domain.LocalProbeConfigDefinition) bool {
	for _, assignment := range definition.Assignments {
		if assignment.AssignmentRevision <= 0 {
			return false
		}
	}
	return true
}

// localConfigHasRegionalApplications lets pre-upgrade documents execute while
// directing the refresher to activate a new revision with complete honest facts.
// This is refresh completeness only, not cutover or send authority.
func localConfigHasRegionalApplications(definition *domain.LocalProbeConfigDefinition) bool {
	if len(definition.RegionalDeliveryApplications) != len(definition.Assignments) {
		return false
	}
	byMonitor := make(map[int64]domain.ProbeDeliveryModeApplication, len(definition.RegionalDeliveryApplications))
	for _, fact := range definition.RegionalDeliveryApplications {
		if !domain.ValidProbeDeliveryModeApplication(fact) || fact.ProbeConfigTarget != definition.Target ||
			fact.ProbeID != domain.LocalProbeID || fact.StreamID != domain.LocalStreamID ||
			fact.ConfigRevision != definition.Revision || fact.AlertDelivery != domain.AlertDeliveryRegional {
			return false
		}
		if _, duplicate := byMonitor[fact.MonitorID]; duplicate {
			return false
		}
		byMonitor[fact.MonitorID] = fact
	}
	for _, assignment := range definition.Assignments {
		if assignment.Monitor == nil {
			return false
		}
		fact, exists := byMonitor[assignment.Monitor.ID]
		if !exists || fact.Generation != assignment.Generation || fact.AssignmentRevision != assignment.AssignmentRevision {
			return false
		}
	}
	return true
}

// StartEventSubscription coalesces configuration events into one refresh worker.
// Periodic reconciliation repairs missed events and writes from other processes.
// Call once per runtime; cancellation stops both subscribers and the worker.
func (s *LocalProbeConfigRefreshService) StartEventSubscription(ctx context.Context, bus ports.EventBus) {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		dirty := make(chan struct{}, 1)
		if bus != nil {
			for _, eventType := range []string{"monitor.update", "monitor.delete", "config.refresh", "notification.update", "notification.delete"} {
				ch := bus.Subscribe(eventType)
				go func() {
					for {
						select {
						case <-ctx.Done():
							return
						case _, ok := <-ch:
							if !ok {
								return
							}
							select {
							case dirty <- struct{}{}:
							default:
							}
						}
					}
				}()
			}
		}
		s.mu.Lock()
		interval := s.reconcileInterval
		s.mu.Unlock()
		if interval <= 0 {
			interval = 5 * time.Second
		}
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				case <-dirty:
				}
				if _, err := s.Refresh(ctx); err != nil && ctx.Err() == nil {
					slog.Error("probe configuration refresh failed; will retry", "error", err)
				}
			}
		}()
	})
}
