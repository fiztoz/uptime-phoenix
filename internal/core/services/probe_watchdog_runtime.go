package services

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const watchdogInboxLimit = 16

type watchdogOwnerEvent struct {
	input      ProbeWatchdogInput
	generation int64
}

// ProbeWatchdogRuntime retains timing across individual sessions. Admission and
// tick reservation share a short gate; all config/storage work happens outside it.
// One Run owns the source controller. Configure dependencies before starting it.
type ProbeWatchdogRuntime struct {
	repo       ports.ProbeWatchdogRepository
	configs    ports.EdgeConfigReader
	authority  func(context.Context) (domain.ProbeWatchdogAuthority, error)
	source     *ProbeWatchdogSource
	origin     time.Time
	now        func() time.Time
	interval   time.Duration
	mu         sync.Mutex
	generation int64
	connected  bool
	queue      []watchdogOwnerEvent
	interrupt  *watchdogOwnerEvent
	wake       chan struct{}
	started    atomic.Bool
	progress   atomic.Int64
}

var _ ports.ProbeHealthAdmission = (*ProbeWatchdogRuntime)(nil)

// NewProbeWatchdogRuntime requires an applied graph reader and source authority.
// Authority is resolved afresh for each operation; it never comes from wire IDs.
func NewProbeWatchdogRuntime(repo ports.ProbeWatchdogRepository, configs ports.EdgeConfigReader, authority func(context.Context) (domain.ProbeWatchdogAuthority, error), peer string) (*ProbeWatchdogRuntime, error) {
	source, err := NewProbeWatchdogSource(repo, peer)
	if err != nil || configs == nil || authority == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeWatchdogRuntime{repo: repo, configs: configs, authority: authority, source: source,
		origin: time.Now(), now: time.Now, interval: 5 * time.Second, wake: make(chan struct{}, 1)}, nil
}

// Begin follows durable adoption of a strictly newer session generation. It
// interrupts recovery without claiming health, and discards uncommitted old work.
func (r *ProbeWatchdogRuntime) Begin(generation int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation <= 0 || generation <= r.generation {
		return ports.ErrConflict
	}
	r.generation, r.connected = generation, true
	r.queue = nil
	now := r.now()
	r.interrupt = &watchdogOwnerEvent{generation: generation, input: ProbeWatchdogInput{Elapsed: now.Sub(r.origin), At: now, Disconnect: true}}
	r.signal()
	return nil
}

// End interrupts only the current session. A late old close cannot disturb a
// newer generation. Its separate control slot remains available on queue overflow.
func (r *ProbeWatchdogRuntime) End(generation int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.generation || !r.connected {
		return
	}
	r.connected = false
	r.queue = nil
	now := r.now()
	r.interrupt = &watchdogOwnerEvent{input: ProbeWatchdogInput{Elapsed: now.Sub(r.origin), At: now, Disconnect: true}}
	r.signal()
}

// Admit captures application receipt time immediately after socket read, before
// decode and worker dispatch. Only bounded validation runs under the shared gate.
// It retains every accepted healthy/degraded sample and rejects overload explicitly.
func (r *ProbeWatchdogRuntime) Admit(generation int64, decode func(time.Time) (*bool, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if decode == nil || !r.connected || generation != r.generation {
		return ports.ErrConflict
	}
	now := r.now()
	health, err := decode(now)
	if err != nil {
		return err
	}
	if health == nil {
		return nil
	}
	if len(r.queue) >= watchdogInboxLimit {
		return errors.New("watchdog health inbox full")
	}
	value := *health
	r.queue = append(r.queue, watchdogOwnerEvent{generation: generation, input: ProbeWatchdogInput{Elapsed: now.Sub(r.origin), At: now, Health: &value}})
	r.signal()
	return nil
}

func (r *ProbeWatchdogRuntime) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *ProbeWatchdogRuntime) next() watchdogOwnerEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.interrupt != nil {
		event := *r.interrupt
		r.interrupt = nil
		return event
	}
	if len(r.queue) > 0 {
		event := r.queue[0]
		r.queue[0] = watchdogOwnerEvent{}
		r.queue = r.queue[1:]
		return event
	}
	now := r.now()
	return watchdogOwnerEvent{input: ProbeWatchdogInput{Elapsed: now.Sub(r.origin), At: now}}
}

func (r *ProbeWatchdogRuntime) current(event watchdogOwnerEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return event.generation == 0 || r.connected && event.generation == r.generation
}

func (r *ProbeWatchdogRuntime) waiting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.interrupt != nil || len(r.queue) > 0
}

// ProgressHealthy bounds owner renewal by completed watchdog storage work. Before
// first application, a successful read proving unarmed state also counts. Merely
// evaluating a timer or dequeuing work never advances this deadline.
func (r *ProbeWatchdogRuntime) ProgressHealthy() bool {
	return r.now().Sub(r.origin)-time.Duration(r.progress.Load()) <= 20*time.Second
}

func (r *ProbeWatchdogRuntime) step(ctx context.Context, event watchdogOwnerEvent) error {
	if !r.current(event) {
		return nil
	}
	authority, err := r.authority(ctx)
	if err != nil {
		return err
	}
	authority.HealthGeneration = event.generation
	config, err := r.configs.Load(ctx)
	if errors.Is(err, ports.ErrNotFound) {
		// A new enrolled probe can retain connector ownership while awaiting its
		// first config. Previously armed state cannot silently become startup.
		state, readErr := r.repo.ReadWatchdog(ctx, authority)
		if readErr != nil {
			return readErr
		}
		if state.Checkpoint.Armed || state.Incident != nil && state.Incident.Status != domain.AlertStatusResolved {
			return ports.ErrConflict
		}
	} else if err != nil {
		return err
	} else {
		if _, err := r.source.Step(ctx, authority, config, event.input); err != nil {
			return err
		}
	}
	r.progress.Store(int64(r.now().Sub(r.origin)))
	return nil
}

// Run processes one event at a time with bounded storage work. Failed proposals
// retain their original timestamp and generation for retry; stale session work
// is discarded, never relabeled as an offline tick. Cancellation joins in callers.
func (r *ProbeWatchdogRuntime) Run(ctx context.Context, report func(error)) error {
	if !r.started.CompareAndSwap(false, true) {
		return ports.ErrConflict
	}
	var pending *watchdogOwnerEvent
	for ctx.Err() == nil {
		if pending == nil || !r.current(*pending) {
			event := r.next()
			pending = &event
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := r.step(opCtx, *pending)
		cancel()
		if err == nil || errors.Is(err, ports.ErrNotFound) {
			// An unenrolled installation cannot retain a startup tick until much
			// later enrollment and thereby backdate its first arming baseline.
			pending = nil
		} else if ctx.Err() == nil && report != nil {
			report(err)
		}
		if err == nil && r.waiting() {
			continue
		}
		pause := r.interval
		if err != nil {
			pause = time.Second
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-r.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
	return ctx.Err()
}
