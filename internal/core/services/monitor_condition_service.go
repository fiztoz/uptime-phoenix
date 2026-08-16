package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	conditionTransitionSamples = 2
	conditionHysteresisPoints  = 5.0
)

type conditionNotifier interface {
	DispatchTracked(ctx context.Context, monitor *domain.Monitor, alert domain.AlertContext) (bool, error)
}

// MonitorConditionService persists typed auxiliary observations and emits
// warning/error/recovery notifications without changing heartbeat status.
type MonitorConditionService struct {
	repo        ports.MonitorConditionRepository
	notifier    conditionNotifier
	maintenance maintenanceChecker
	bus         ports.EventBus
	now         func() time.Time
	mu          sync.Mutex
	locks       map[string]*sync.Mutex
}

// NewMonitorConditionService creates the capacity-condition lifecycle service.
func NewMonitorConditionService(
	repo ports.MonitorConditionRepository,
	notifier conditionNotifier,
	maintenance maintenanceChecker,
	bus ports.EventBus,
) *MonitorConditionService {
	return &MonitorConditionService{
		repo:        repo,
		notifier:    notifier,
		maintenance: maintenance,
		bus:         bus,
		now:         time.Now,
		locks:       map[string]*sync.Mutex{},
	}
}

// OnCheck consumes typed observations from the owning heartbeat worker. All
// failures are logged and kept off the availability path.
func (s *MonitorConditionService) OnCheck(ctx context.Context, monitor *domain.Monitor, observations []domain.ConditionObservation) {
	if s == nil || s.repo == nil || monitor == nil || len(observations) == 0 {
		return
	}
	for i := range observations {
		if err := s.record(ctx, monitor, observations[i]); err != nil {
			slog.Error("monitor condition: record failed",
				"monitor_id", monitor.ID,
				"kind", observations[i].Kind,
				"error", err,
			)
		}
	}
}

// ListAll returns all latest conditions. Authorization belongs to the caller.
func (s *MonitorConditionService) ListAll(ctx context.Context) ([]*domain.MonitorCondition, error) {
	return s.repo.ListAll(ctx)
}

// ListByMonitorIDs returns latest conditions for a scoped monitor set.
func (s *MonitorConditionService) ListByMonitorIDs(ctx context.Context, monitorIDs []int64) ([]*domain.MonitorCondition, error) {
	return s.repo.ListByMonitorIDs(ctx, monitorIDs)
}

func (s *MonitorConditionService) record(ctx context.Context, monitor *domain.Monitor, observation domain.ConditionObservation) error {
	if observation.Kind == "" {
		return fmt.Errorf("condition kind is required")
	}
	if !observation.State.IsValid() {
		return fmt.Errorf("invalid condition state %q", observation.State)
	}

	now := s.now().UTC()
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	} else {
		observation.ObservedAt = observation.ObservedAt.UTC()
	}
	observation.StaleAfter = conditionStaleAfter(observation.ObservedAt, monitor.Interval)

	unlock := s.lockKind(monitor.ID, observation.Kind)
	defer unlock()

	previous, err := s.repo.Get(ctx, monitor.ID, observation.Kind)
	if err != nil && !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("load previous: %w", err)
	}
	if err != nil {
		previous = nil
	}

	stable := conditionStableState(previous)
	applyConditionHysteresis(stable, &observation)
	candidate := observation.State

	condition := &domain.MonitorCondition{
		MonitorID:            monitor.ID,
		ConditionObservation: observation,
		ConsecutiveState:     candidate,
		ConsecutiveCount:     1,
	}
	if candidate != domain.ConditionStateError {
		lastSuccess := observation.ObservedAt
		condition.LastSuccessAt = &lastSuccess
	}
	if previous != nil {
		condition.LastNotifiedState = previous.LastNotifiedState
		condition.LastNotifiedAt = previous.LastNotifiedAt
		if condition.LastSuccessAt == nil {
			condition.LastSuccessAt = previous.LastSuccessAt
		}
		if previous.ConsecutiveState == candidate {
			condition.ConsecutiveCount = previous.ConsecutiveCount + 1
		}
	}

	// First-ever OK is immediately stable. First-ever warning/error stays
	// unconfirmed (empty State) until two consecutive samples.
	if previous == nil && candidate == domain.ConditionStateOK {
		condition.ConsecutiveCount = conditionTransitionSamples
	}
	if condition.ConsecutiveCount >= conditionTransitionSamples {
		condition.State = candidate
	} else {
		condition.State = stable
	}

	if err := s.repo.Upsert(ctx, condition); err != nil {
		return fmt.Errorf("persist observation: %w", err)
	}
	if condition.State != "" {
		s.publish(ctx, condition)
	}

	if condition.ConsecutiveCount < conditionTransitionSamples || !shouldNotifyCondition(condition) {
		return nil
	}
	if s.maintenance != nil {
		active, maintenanceErr := s.maintenance.IsActive(ctx, monitor.ID)
		if maintenanceErr != nil {
			slog.Warn("monitor condition: maintenance check failed, continuing",
				"monitor_id", monitor.ID, "error", maintenanceErr)
		} else if active {
			return nil
		}
	}
	if s.notifier == nil {
		return nil
	}

	alert := conditionAlert(monitor, previous, condition, now)
	delivered, dispatchErr := s.notifier.DispatchTracked(ctx, monitor, alert)
	if dispatchErr != nil {
		if !delivered {
			return fmt.Errorf("dispatch: %w", dispatchErr)
		}
		slog.Warn("monitor condition: some notification channels failed",
			"monitor_id", monitor.ID, "kind", condition.Kind, "error", dispatchErr)
	}
	if !delivered {
		return nil
	}
	condition.LastNotifiedState = condition.State
	condition.LastNotifiedAt = &now
	if err := s.repo.Upsert(ctx, condition); err != nil {
		return fmt.Errorf("persist notification cursor: %w", err)
	}
	s.publish(ctx, condition)
	return nil
}

func conditionStableState(previous *domain.MonitorCondition) domain.ConditionState {
	if previous == nil {
		return ""
	}
	return previous.State
}

func applyConditionHysteresis(stable domain.ConditionState, observation *domain.ConditionObservation) {
	if observation == nil || stable != domain.ConditionStateWarning ||
		observation.State != domain.ConditionStateOK || observation.Percent == nil || observation.Threshold == nil {
		return
	}
	clearBelow := math.Max(0, *observation.Threshold-conditionHysteresisPoints)
	if *observation.Percent >= clearBelow {
		observation.State = domain.ConditionStateWarning
		observation.Message = fmt.Sprintf(
			"%s remains in warning until usage falls below %.1f%%",
			conditionLabel(observation.Resource, observation.Kind), clearBelow,
		)
	}
}

func shouldNotifyCondition(current *domain.MonitorCondition) bool {
	if current == nil || current.State == "" {
		return false
	}
	switch current.State {
	case domain.ConditionStateWarning, domain.ConditionStateError:
		return current.LastNotifiedState != current.State
	case domain.ConditionStateOK:
		return current.LastNotifiedState == domain.ConditionStateWarning || current.LastNotifiedState == domain.ConditionStateError
	default:
		return false
	}
}

func (s *MonitorConditionService) lockKind(monitorID int64, kind string) func() {
	key := fmt.Sprintf("%d/%s", monitorID, kind)
	s.mu.Lock()
	if s.locks == nil {
		s.locks = map[string]*sync.Mutex{}
	}
	lk, ok := s.locks[key]
	if !ok {
		lk = &sync.Mutex{}
		s.locks[key] = lk
	}
	s.mu.Unlock()
	lk.Lock()
	return lk.Unlock
}

func conditionAlert(monitor *domain.Monitor, previous, current *domain.MonitorCondition, now time.Time) domain.AlertContext {
	previousState := domain.ConditionStateOK
	if previous != nil && previous.State != "" {
		previousState = previous.State
	}
	observedAt := current.ObservedAt.UTC()
	message := current.Message
	if current.State == domain.ConditionStateOK {
		message = fmt.Sprintf("%s recovered to normal", conditionLabel(current.Resource, current.Kind))
		if current.Percent != nil {
			message = fmt.Sprintf("%s recovered to %.1f%%", conditionLabel(current.Resource, current.Kind), *current.Percent)
		}
	}
	return domain.AlertContext{
		AlertScope:             domain.AlertScopeMonitor,
		MonitorID:              monitor.ID,
		MonitorName:            monitor.Name,
		MonitorType:            monitor.Type,
		MonitorTarget:          monitor.Target(),
		MonitorDescription:     monitor.Description,
		MonitorOwner:           monitor.Owner,
		Status:                 domain.StatusUp,
		PreviousStatus:         domain.StatusUp,
		Message:                message,
		StartedAt:              now,
		EventKind:              domain.AlertEventCapacityCondition,
		ConditionKind:          current.Kind,
		ConditionState:         current.State,
		ConditionPreviousState: previousState,
		ConditionUsed:          current.Used,
		ConditionLimit:         current.Limit,
		ConditionPercent:       current.Percent,
		ConditionThreshold:     current.Threshold,
		ConditionUnit:          current.Unit,
		ConditionResource:      current.Resource,
		ConditionScope:         current.Scope,
		ConditionSource:        current.Source,
		ConditionObservedAt:    &observedAt,
	}
}

func conditionStaleAfter(observedAt time.Time, intervalSeconds int) time.Time {
	interval := time.Duration(intervalSeconds) * time.Second
	if interval < time.Minute {
		interval = time.Minute
	}
	return observedAt.Add(3 * interval)
}

func conditionLabel(resource, kind string) string {
	if resource != "" {
		return resource
	}
	if kind == domain.MonitorConditionSessionPool {
		return "Session pool"
	}
	return "Capacity"
}

func (s *MonitorConditionService) publish(ctx context.Context, condition *domain.MonitorCondition) {
	if s.bus != nil {
		_ = s.bus.Publish(ctx, ports.Event{Type: "condition.update", Payload: condition})
	}
}
