package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// EdgeScheduler executes only accepted edge assignments, with a fixed maximum
// of 32 checks in flight. It never reads mutable hub configuration or sends pages.
type EdgeScheduler struct {
	configs  ports.EdgeConfigReader
	recorder *services.EdgeRecordingService
	checker  func(string) (ports.Checker, bool)
	cron     ports.CronEvaluator
	healthy  atomic.Bool
	revision atomic.Int64
	running  atomic.Bool
}

// NewEdgeScheduler wires real checker adapters to source-owned recording.
func NewEdgeScheduler(configs ports.EdgeConfigReader, recorder *services.EdgeRecordingService, checker func(string) (ports.Checker, bool), cron ports.CronEvaluator) *EdgeScheduler {
	return &EdgeScheduler{configs: configs, recorder: recorder, checker: checker, cron: cron}
}

// Diagnostics reports local execution health independently of hub connectivity.
func (s *EdgeScheduler) Diagnostics() (bool, int64) { return s.healthy.Load(), s.revision.Load() }

type edgeRunningCheck struct {
	revision, generation int64
	cancel               context.CancelFunc
}
type edgeCompletedCheck struct {
	id, revision, generation int64
	next                     time.Time
	err                      error
}

// Run polls the durable accepted graph and waits for all canceled checks before
// returning. Repeated/concurrent Run calls fail rather than creating duplicate owners.
func (s *EdgeScheduler) Run(ctx context.Context, report func(error)) error {
	return s.RunUntilQuiesced(ctx, nil, report)
}

// RunUntilQuiesced stops admitting checks when quiesce closes, then joins and
// records in-flight work. The caller bounds that grace by canceling ctx.
func (s *EdgeScheduler) RunUntilQuiesced(ctx context.Context, quiesce <-chan struct{}, report func(error)) error {
	if !s.running.CompareAndSwap(false, true) {
		return errors.New("edge scheduler already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait(); s.healthy.Store(false) }()
	completed := make(chan edgeCompletedCheck, 32)
	inFlight := make(map[int64]edgeRunningCheck)
	nextRun := make(map[int64]time.Time)
	failed := make(map[int64]bool)
	var config *domain.EdgeResolvedConfig
	var nextLoad time.Time
	quiescing := false
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-quiesce:
			quiescing = true
			quiesce = nil
			s.healthy.Store(false)
		default:
		}
		if quiescing && len(inFlight) == 0 {
			return nil
		}
		now := time.Now().UTC()
		if !quiescing && !now.Before(nextLoad) {
			loaded, err := s.configs.Load(runCtx)
			nextLoad = now.Add(5 * time.Second)
			if err != nil || loaded == nil {
				s.healthy.Store(false)
				config = nil
				for _, check := range inFlight {
					check.cancel()
				}
				if err != nil && !errors.Is(err, ports.ErrNotFound) && report != nil {
					report(err)
				}
			} else {
				if config == nil || config.Metadata.Revision != loaded.Metadata.Revision {
					for _, check := range inFlight {
						check.cancel()
					}
					nextRun = make(map[int64]time.Time)
					failed = make(map[int64]bool)
				}
				config = loaded
				s.revision.Store(config.Metadata.Revision)
				s.healthy.Store(len(failed) == 0)
			}
		}
		if !quiescing && config != nil {
			for _, a := range config.Assignments {
				select {
				case <-quiesce:
					quiescing = true
				default:
				}
				if quiescing || runCtx.Err() != nil {
					break
				}
				if len(inFlight) >= 32 {
					break
				}
				m := a.Monitor
				if m == nil || !m.Active || m.Interval <= 0 {
					continue
				}
				if _, busy := inFlight[m.ID]; busy || now.Before(nextRun[m.ID]) {
					continue
				}
				checkCtx, checkCancel := context.WithCancel(runCtx)
				inFlight[m.ID] = edgeRunningCheck{revision: config.Metadata.Revision, generation: a.Generation, cancel: checkCancel}
				workers.Add(1)
				go func(cfg *domain.EdgeResolvedConfig, assignment domain.EdgeResolvedAssignment) {
					defer workers.Done()
					defer checkCancel()
					done := s.execute(checkCtx, cfg, assignment)
					select {
					case completed <- done:
					case <-runCtx.Done():
					}
				}(config, a)
			}
		}
		select {
		case <-quiesce:
			quiescing = true
			quiesce = nil
			s.healthy.Store(false)
		case <-ctx.Done():
			return ctx.Err()
		case done := <-completed:
			delete(inFlight, done.id)
			if config != nil && done.revision == config.Metadata.Revision {
				nextRun[done.id] = done.next
				if done.err == nil {
					delete(failed, done.id)
				} else if !errors.Is(done.err, context.Canceled) && !errors.Is(done.err, ports.ErrConflict) {
					failed[done.id] = true
				}
				s.healthy.Store(!quiescing && len(failed) == 0)
			}
			if done.err != nil && !errors.Is(done.err, context.Canceled) && !errors.Is(done.err, ports.ErrConflict) {
				if report != nil {
					report(done.err)
				}
			}
		case <-ticker.C:
		}
	}
}

func (s *EdgeScheduler) execute(ctx context.Context, config *domain.EdgeResolvedConfig, assignment domain.EdgeResolvedAssignment) edgeCompletedCheck {
	m := assignment.Monitor
	done := edgeCompletedCheck{id: m.ID, revision: config.Metadata.Revision, generation: assignment.Generation}
	interval := time.Duration(m.Interval) * time.Second
	maintenance, err := services.EdgeMaintenanceActive(config, assignment, s.cron, time.Now().UTC())
	result := ports.CheckResult{Status: domain.StatusUp}
	if err == nil && !maintenance {
		provider, exists := s.checker(m.Type)
		if !exists || provider == nil {
			err = domain.ErrValidation
		} else {
			settings := checkConfigForMonitor(m)
			settings["_proxy"] = proxyCheckConfig(assignment.Proxy)
			timeout := time.Duration(m.Timeout * float64(time.Second))
			if timeout <= 0 {
				timeout = 30 * time.Second
			}
			checkCtx, cancel := context.WithTimeout(ctx, timeout)
			var checkErr error
			result, checkErr = provider.Check(checkCtx, settings)
			cancel()
			if checkErr != nil {
				result = ports.CheckResult{Status: domain.StatusDown}
			}
			if m.UpsideDown {
				if result.Status == domain.StatusDown {
					result.Status = domain.StatusUp
				} else if result.Status == domain.StatusUp {
					result.Status = domain.StatusDown
				}
			}
		}
	}
	if err == nil && ctx.Err() == nil {
		// Checker diagnostics may contain URL credentials or response bodies. The
		// engineering runtime persists bounded availability text, never raw output.
		result.Message = m.Type + " check " + result.Status.String()
		recordCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var observation domain.RegionalObservation
		observation, err = s.recorder.Record(recordCtx, config, assignment, result, time.Now().UTC())
		cancel()
		if err == nil && observation.Status == domain.StatusPending && m.RetryInterval > 0 {
			interval = time.Duration(m.RetryInterval) * time.Second
		}
	} else if ctx.Err() != nil {
		err = ctx.Err()
	}
	done.next, done.err = time.Now().UTC().Add(interval), err
	return done
}
