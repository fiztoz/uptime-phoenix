// Package scheduler provides adapters for the ports.Scheduler interface.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// LocalScheduler runs monitor checks in-process using a ticker.
// Phase 1 default — zero external dependencies.
type LocalScheduler struct {
	monitorRepo    ports.MonitorRepository
	heartbeatRepo  ports.HeartbeatRepository
	checkerFn      func(string) (ports.Checker, bool)
	heartbeatSvc   *services.HeartbeatService
	maintenanceSvc *services.MaintenanceService
	logger         *slog.Logger
	stopCh         chan struct{}
	lastCheck      sync.Map // monitorID int64 -> time.Time
	proxyResolver  *proxyResolver
	slots          *checkSlots
}

// NewLocalScheduler creates a new in-process scheduler.
func NewLocalScheduler(
	monitorRepo ports.MonitorRepository,
	heartbeatRepo ports.HeartbeatRepository,
	checkerFn func(string) (ports.Checker, bool),
	heartbeatSvc *services.HeartbeatService,
	maintenanceSvc *services.MaintenanceService,
	logger *slog.Logger,
) *LocalScheduler {
	return &LocalScheduler{
		monitorRepo:    monitorRepo,
		heartbeatRepo:  heartbeatRepo,
		checkerFn:      checkerFn,
		heartbeatSvc:   heartbeatSvc,
		maintenanceSvc: maintenanceSvc,
		logger:         logger,
		stopCh:         make(chan struct{}),
		proxyResolver:  newProxyResolver(nil),
		slots:          newCheckSlots(defaultCheckConcurrency),
	}
}

// SetProxyRepo attaches a proxy repository so monitors with ProxyID set
// route their checks through the configured proxy. Optional: when never
// called (or called with nil), proxy_id is ignored and checks run direct.
// Must be called before Run starts claiming/checking monitors to avoid a
// race on the first few ticks; run.go calls it immediately after construction.
func (s *LocalScheduler) SetProxyRepo(repo ports.ProxyRepository) {
	s.proxyResolver.setRepo(repo)
}

// Run starts the scheduler loop. Blocks until ctx is canceled.
func (s *LocalScheduler) Run(ctx context.Context) error {
	s.logger.Info("scheduler starting")
	defer s.logger.Info("scheduler stopped")

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick is called every second and runs checks for monitors that are due.
func (s *LocalScheduler) tick(ctx context.Context) {
	monitors, err := s.monitorRepo.ListActive(ctx)
	if err != nil {
		s.logger.Error("scheduler: failed to list active monitors", "error", err)
		return
	}

	now := time.Now().UTC()

	for _, m := range monitors {
		if !s.shouldRun(m, now) {
			continue
		}

		s.lastCheck.Store(m.ID, now)
		s.startCheck(ctx, m)
	}
}

func (s *LocalScheduler) startCheck(ctx context.Context, m *domain.Monitor) {
	go func() {
		if !s.slots.acquire(ctx) {
			return
		}
		defer s.slots.release()
		s.runCheck(ctx, m)
	}()
}

// checkConfigForMonitor builds the config map passed to checkers, merging
// per-monitor settings (e.g. accepted status codes) with type-specific config.
func checkConfigForMonitor(m *domain.Monitor) map[string]any {
	cfg := make(map[string]any, len(m.Config)+2)
	for k, v := range m.Config {
		cfg[k] = v
	}
	if len(m.AcceptedStatusCodes) > 0 {
		codes := make([]any, len(m.AcceptedStatusCodes))
		for i, c := range m.AcceptedStatusCodes {
			codes[i] = c
		}
		cfg["accepted_statuscodes"] = codes
	}
	if m.TLSIgnore {
		cfg["tls_ignore"] = true
	}
	if m.Timeout > 0 {
		cfg["timeout"] = m.Timeout
	}
	return cfg
}

// shouldRun checks whether enough time has elapsed since the last check.
func (s *LocalScheduler) shouldRun(m *domain.Monitor, now time.Time) bool {
	if m.Interval <= 0 {
		return false
	}
	interval := time.Duration(m.Interval) * time.Second

	v, ok := s.lastCheck.Load(m.ID)
	if !ok {
		// Never checked — always run.
		return true
	}
	lastTime := v.(time.Time)
	return now.Sub(lastTime) >= interval
}

// runCheck executes a single monitor check in a goroutine with panic recovery.
func (s *LocalScheduler) runCheck(ctx context.Context, m *domain.Monitor) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("scheduler: panic in check goroutine",
				"monitor_id", m.ID,
				"monitor_type", m.Type,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()

	// Check maintenance window first — if active, record MAINTENANCE status and skip real check.
	if s.maintenanceSvc != nil {
		if active, _ := s.maintenanceSvc.IsActive(ctx, m.ID); active {
			if err := s.heartbeatSvc.Record(heartbeatRecordContext(), m, ports.CheckResult{
				Status:  domain.StatusMaintenance,
				Message: "maintenance window active",
			}); err != nil {
				s.logger.Warn("scheduler: record maintenance heartbeat failed", "monitor_id", m.ID, "error", err)
			}
			return
		}
	}

	// Create a timeout context for this check.
	timeout := time.Duration(m.Timeout * float64(time.Second))
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Look up the checker for this monitor type.
	checker, ok := s.checkerFn(m.Type)
	if !ok {
		s.logger.Warn("scheduler: no checker registered for monitor type",
			"monitor_id", m.ID,
			"type", m.Type,
		)
		return
	}

	// Perform the check. Merge monitor-level fields into config for checkers.
	// If m.ProxyID is set, inject the resolved proxy under "_proxy" (nil
	// otherwise, which checkers safely ignore) — the checker never sees the
	// monitor, only this config map, so resolution has to happen here (see
	// proxy_resolver.go).
	checkConfig := checkConfigForMonitor(m)
	checkConfig["_proxy"] = s.proxyResolver.configFor(ctx, m)
	result, err := checker.Check(checkCtx, checkConfig)
	if err != nil {
		s.logger.Error("scheduler: check failed",
			"monitor_id", m.ID,
			"type", m.Type,
			"error", err,
		)
		// Check returned an error — treat as DOWN.
		result = ports.CheckResult{
			Status:  domain.StatusDown,
			Message: err.Error(),
		}
	}

	// Apply upside-down mode.
	if m.UpsideDown {
		switch result.Status {
		case domain.StatusUp:
			result.Status = domain.StatusDown
		case domain.StatusDown:
			result.Status = domain.StatusUp
		}
	}

	// Record the heartbeat on a fresh context — never reuse the check timeout context.
	if recordErr := s.heartbeatSvc.Record(heartbeatRecordContext(), m, result); recordErr != nil {
		s.logger.Error("scheduler: failed to record heartbeat",
			"monitor_id", m.ID,
			"error", recordErr,
		)
	}
}
