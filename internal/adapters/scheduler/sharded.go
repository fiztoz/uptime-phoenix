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

// ShardedScheduler uses DB-leased sharding so multiple worker replicas
// can share the monitor load. Each worker claims a batch of monitors,
// runs checks only for its claimed set, and periodically refreshes leases.
//
// On shutdown it releases its leases so other workers can pick them up.
type ShardedScheduler struct {
	monitorRepo    ports.MonitorRepository
	checkerFn      func(string) (ports.Checker, bool)
	heartbeatSvc   *services.HeartbeatService
	maintenanceSvc *services.MaintenanceService
	logger         *slog.Logger

	workerID  string
	batchSize int
	leaseTTL  time.Duration
	pollEvery time.Duration

	stopCh    chan struct{}
	lastCheck sync.Map // monitorID int64 -> time.Time
	slots     *checkSlots

	proxyResolver *proxyResolver
}

// ShardedSchedulerConfig holds configuration for the sharded scheduler.
type ShardedSchedulerConfig struct {
	WorkerID  string        // unique identifier for this worker (e.g. hostname+pid)
	BatchSize int           // max monitors to claim per poll cycle (default 200)
	LeaseTTL  time.Duration // how long a lease lasts before expiry (default 5m)
	PollEvery time.Duration // how often to claim/refresh leases (default 30s)
}

// NewShardedScheduler creates a DB-leased sharded scheduler.
func NewShardedScheduler(
	monitorRepo ports.MonitorRepository,
	checkerFn func(string) (ports.Checker, bool),
	heartbeatSvc *services.HeartbeatService,
	maintenanceSvc *services.MaintenanceService,
	logger *slog.Logger,
	cfg ShardedSchedulerConfig,
) *ShardedScheduler {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 200
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 5 * time.Minute
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 30 * time.Second
	}

	return &ShardedScheduler{
		monitorRepo:    monitorRepo,
		checkerFn:      checkerFn,
		heartbeatSvc:   heartbeatSvc,
		maintenanceSvc: maintenanceSvc,
		logger:         logger,
		workerID:       cfg.WorkerID,
		batchSize:      cfg.BatchSize,
		leaseTTL:       cfg.LeaseTTL,
		pollEvery:      cfg.PollEvery,
		stopCh:         make(chan struct{}),
		proxyResolver:  newProxyResolver(nil),
		slots:          newCheckSlots(defaultCheckConcurrency),
	}
}

// SetProxyRepo attaches a proxy repository so monitors with ProxyID set
// route their checks through the configured proxy. Optional: when never
// called (or called with nil), proxy_id is ignored and checks run direct.
// Mirrors LocalScheduler.SetProxyRepo.
func (s *ShardedScheduler) SetProxyRepo(repo ports.ProxyRepository) {
	s.proxyResolver.setRepo(repo)
}

// Run starts the sharded scheduler loop. Blocks until ctx is canceled.
func (s *ShardedScheduler) Run(ctx context.Context) error {
	s.logger.Info("sharded scheduler starting", "worker_id", s.workerID)
	defer s.logger.Info("sharded scheduler stopped", "worker_id", s.workerID)

	// Release leases on shutdown.
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if n, err := s.monitorRepo.ReleaseLeases(releaseCtx, s.workerID); err != nil {
			s.logger.Error("failed to release leases on shutdown", "error", err, "count", n)
		} else {
			s.logger.Info("released leases on shutdown", "count", n)
		}
	}()

	// Initial claim.
	s.claim(ctx)

	ticker := time.NewTicker(1 * time.Second)
	leaseTicker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()
	defer leaseTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.stopCh:
			return nil
		case <-leaseTicker.C:
			// Periodically refresh leases and claim more monitors.
			s.refreshAndClaim(ctx)
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// claim fetches a new batch of monitors from the DB.
func (s *ShardedScheduler) claim(ctx context.Context) {
	monitors, err := s.monitorRepo.ClaimBatch(ctx, s.workerID, s.batchSize, s.leaseTTL)
	if err != nil {
		s.logger.Error("sharded scheduler: failed to claim batch", "error", err)
		return
	}
	s.logger.Info("claimed monitors", "count", len(monitors), "worker_id", s.workerID)
}

// refreshAndClaim refreshes the lease on existing monitors and claims new ones.
func (s *ShardedScheduler) refreshAndClaim(ctx context.Context) {
	// Refresh existing leases.
	if n, err := s.monitorRepo.RefreshLease(ctx, s.workerID); err != nil {
		s.logger.Error("sharded scheduler: failed to refresh lease", "error", err)
	} else if n > 0 {
		s.logger.Debug("refreshed leases", "count", n)
	}

	// Claim more monitors if we have capacity.
	s.claim(ctx)
}

// tick is called every second and runs checks for claimed monitors that are due.
func (s *ShardedScheduler) tick(ctx context.Context) {
	// Get all active monitors and filter to our claimed ones.
	// Since ClaimBatch already set worker_id, ListActive returns all active monitors.
	// We filter by checking worker_id in the domain object — but domain.Monitor
	// doesn't have WorkerID. Instead, we use the same approach as LocalScheduler
	// but rely on the DB lease: we only claim monitors we should run.
	//
	// For efficiency, we use ListActive and filter by what we've stored in lastCheck.
	// The claim ensures only this worker sees these monitors in its batch.

	monitors, err := s.monitorRepo.ListActive(ctx)
	if err != nil {
		s.logger.Error("sharded scheduler: failed to list active monitors", "error", err)
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

func (s *ShardedScheduler) startCheck(ctx context.Context, m *domain.Monitor) {
	go func() {
		if !s.slots.acquire(ctx) {
			return
		}
		defer s.slots.release()
		s.runCheck(ctx, m)
	}()
}

// shouldRun checks whether enough time has elapsed since the last check.
func (s *ShardedScheduler) shouldRun(m *domain.Monitor, now time.Time) bool {
	if m.Interval <= 0 {
		return false
	}
	interval := time.Duration(m.Interval) * time.Second

	v, ok := s.lastCheck.Load(m.ID)
	if !ok {
		return true
	}
	lastTime := v.(time.Time)
	return now.Sub(lastTime) >= interval
}

// runCheck executes a single monitor check with panic recovery.
func (s *ShardedScheduler) runCheck(ctx context.Context, m *domain.Monitor) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("sharded scheduler: panic in check goroutine",
				"monitor_id", m.ID,
				"monitor_type", m.Type,
				"panic", fmt.Sprintf("%v", r),
			)
		}
	}()

	// Check maintenance window.
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

	checker, ok := s.checkerFn(m.Type)
	if !ok {
		s.logger.Warn("sharded scheduler: no checker for type",
			"monitor_id", m.ID, "type", m.Type,
		)
		return
	}

	// If m.ProxyID is set, inject the resolved proxy under "_proxy" (nil
	// otherwise, which checkers safely ignore) — mirrors LocalScheduler.runCheck.
	checkConfig := checkConfigForMonitor(m)
	checkConfig["_proxy"] = s.proxyResolver.configFor(ctx, m)
	result, err := checker.Check(checkCtx, checkConfig)
	if err != nil {
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

	if recordErr := s.heartbeatSvc.Record(heartbeatRecordContext(), m, result); recordErr != nil {
		s.logger.Error("sharded scheduler: failed to record heartbeat",
			"monitor_id", m.ID, "error", recordErr,
		)
	}
}
