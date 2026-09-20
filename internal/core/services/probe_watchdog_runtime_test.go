package services

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type watchdogConfigReader func(context.Context) (*domain.EdgeResolvedConfig, error)

func (f watchdogConfigReader) Load(ctx context.Context) (*domain.EdgeResolvedConfig, error) {
	return f(ctx)
}

func watchdogRuntimeFixture(t *testing.T) (*ProbeWatchdogRuntime, *watchdogSourceRepo, *atomic.Int64) {
	t.Helper()
	_, repo, authority, config := watchdogSourceFixture(t)
	r, err := NewProbeWatchdogRuntime(repo, watchdogConfigReader(func(context.Context) (*domain.EdgeResolvedConfig, error) { return config, nil }), func(context.Context) (domain.ProbeWatchdogAuthority, error) { return authority, nil }, "hub")
	if err != nil {
		t.Fatal(err)
	}
	seconds := &atomic.Int64{}
	r.origin = watchdogTestEpoch
	r.now = func() time.Time { return r.origin.Add(time.Duration(seconds.Load()) * time.Second) }
	return r, repo, seconds
}

func admitWatchdog(t *testing.T, r *ProbeWatchdogRuntime, generation int64, healthy bool) {
	t.Helper()
	if err := r.Admit(generation, func(time.Time) (*bool, error) { return &healthy, nil }); err != nil {
		t.Fatal(err)
	}
}

func waitWatchdogSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog operation did not complete")
	}
}

func TestProbeWatchdogRuntimeOrdersAdmissionAndTicks(t *testing.T) {
	r, _, seconds := watchdogRuntimeFixture(t)
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	r.next() // Consume the initial recovery interruption.
	seconds.Store(10)
	entered, release, admitted := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	go func() {
		defer close(admitted)
		err := r.Admit(1, func(at time.Time) (*bool, error) {
			if !at.Equal(watchdogTestEpoch.Add(10 * time.Second)) {
				t.Error("timestamp recaptured after decode")
			}
			close(entered)
			<-release
			good := true
			return &good, nil
		})
		if err != nil {
			t.Error(err)
		}
	}()
	waitWatchdogSignal(t, entered)
	seconds.Store(15)
	reserved := make(chan watchdogOwnerEvent, 1)
	go func() { reserved <- r.next() }()
	// Use a second channel to release the decoder without a sleep; next must
	// observe its already-reserved health receipt before reserving a later tick.
	release <- struct{}{}
	waitWatchdogSignal(t, admitted)
	select {
	case event := <-reserved:
		if event.input.Health == nil || !*event.input.Health || event.input.Elapsed != 10*time.Second || event.generation != 1 {
			t.Fatal("tick passed an admitted health frame", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reservation blocked")
	}
	if event := r.next(); event.input.Elapsed != 15*time.Second || event.input.Health != nil {
		t.Fatal("tick receipt order changed", event)
	}
}

func TestProbeWatchdogRuntimeGenerationOverflowAndLateClose(t *testing.T) {
	r, _, seconds := watchdogRuntimeFixture(t)
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	r.next()
	for range watchdogInboxLimit {
		admitWatchdog(t, r, 1, true)
	}
	if err := r.Admit(1, func(time.Time) (*bool, error) { v := false; return &v, nil }); err == nil {
		t.Fatal("overloaded health silently dropped")
	}
	seconds.Store(5)
	r.End(1)
	event := r.next()
	if !event.input.Disconnect || event.generation != 0 || r.waiting() {
		t.Fatal("full inbox prevented interruption", event)
	}
	if err := r.Begin(2); err != nil {
		t.Fatal(err)
	}
	r.End(1)
	called := false
	if err := r.Admit(1, func(time.Time) (*bool, error) { called = true; return nil, nil }); !errors.Is(err, ports.ErrConflict) || called {
		t.Fatal("old reader entered decoder after reconnect", err)
	}
	if event = r.next(); event.generation != 2 || !event.input.Disconnect {
		t.Fatal("old close disturbed new generation")
	}
	admitWatchdog(t, r, 2, false)
	admitWatchdog(t, r, 2, true)
	if event = r.next(); event.input.Health == nil || *event.input.Health {
		t.Fatal("degraded sample was coalesced away")
	}
	if event = r.next(); event.input.Health == nil || !*event.input.Health {
		t.Fatal("healthy sample missing")
	}
}

func TestProbeWatchdogRuntimeProgressRequiresCommitAndAdmissionDoesNotWaitForDB(t *testing.T) {
	r, repo, seconds := watchdogRuntimeFixture(t)
	if err := r.Begin(1); err != nil {
		t.Fatal(err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	repo.beforeCommit = func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return ports.ErrConflict
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	go func() {
		defer close(finished)
		if err := r.step(t.Context(), r.next()); !errors.Is(err, ports.ErrConflict) {
			t.Error(err)
		}
	}()
	waitWatchdogSignal(t, started)
	seconds.Store(21)
	admitted := make(chan struct{})
	go func() { defer close(admitted); admitWatchdog(t, r, 1, true) }()
	waitWatchdogSignal(t, admitted)
	if r.ProgressHealthy() {
		t.Fatal("queued health or attempted commit renewed progress")
	}
	close(release)
	waitWatchdogSignal(t, finished)
	repo.beforeCommit = nil
	if err := r.step(t.Context(), r.next()); err != nil {
		t.Fatal(err)
	}
	if !r.ProgressHealthy() || repo.state.Status != domain.ProbeWatchdogHealthy {
		t.Fatal("successful health checkpoint not published")
	}
}

func TestProbeWatchdogRuntimeMissingConfigOnlyAllowsUnarmedStartup(t *testing.T) {
	r, repo, seconds := watchdogRuntimeFixture(t)
	r.configs = watchdogConfigReader(func(context.Context) (*domain.EdgeResolvedConfig, error) { return nil, ports.ErrNotFound })
	seconds.Store(60)
	if err := r.step(t.Context(), r.next()); err != nil || !r.ProgressHealthy() || repo.state.Version != 0 {
		t.Fatal("new unconfigured installation could not retain ownership", err)
	}
	repo.state.Checkpoint.Armed = true
	seconds.Store(81)
	if err := r.step(t.Context(), r.next()); !errors.Is(err, ports.ErrConflict) || r.ProgressHealthy() {
		t.Fatal("missing config hid previously armed state", err)
	}
}

func TestProbeWatchdogRuntimeEnrollmentDoesNotBackdateArming(t *testing.T) {
	r, repo, seconds := watchdogRuntimeFixture(t)
	_, _, authority, _ := watchdogSourceFixture(t)
	var enrolled atomic.Bool
	failed := make(chan struct{})
	var absence sync.Once
	r.authority = func(context.Context) (domain.ProbeWatchdogAuthority, error) {
		if !enrolled.Load() {
			absence.Do(func() { close(failed) })
			return domain.ProbeWatchdogAuthority{}, ports.ErrNotFound
		}
		return authority, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	repo.beforeCommit = func(context.Context) error {
		calls++
		if calls == 3 {
			cancel()
			return context.Canceled
		}
		return nil
	}
	r.interval = time.Millisecond
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, nil) }()
	waitWatchdogSignal(t, failed)
	seconds.Store(600)
	enrolled.Store(true)
	r.signal()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("enrolled runtime stalled")
	}
	if repo.state.Version != 2 || repo.state.Status != domain.ProbeWatchdogStarting || repo.state.Checkpoint.LossElapsed != 0 || repo.state.Incident != nil || len(repo.deliveries) != 0 {
		t.Fatal("startup retry backdated arming", repo.state)
	}
}

func TestProbeConnectorWatchdogStallCancelsAndJoinsBeforeRelease(t *testing.T) {
	r, repo, seconds := watchdogRuntimeFixture(t)
	started, joined := make(chan struct{}), make(chan struct{})
	repo.beforeCommit = func(ctx context.Context) error { close(started); <-ctx.Done(); close(joined); return ctx.Err() }
	connections, leases := &connectorConnections{}, &connectorLeases{}
	svc := newConnectorTestService(t, connections, leases, connectorTransport{})
	svc.renewInterval = time.Millisecond
	svc.watchdogFactory = func(context.Context, domain.ProbeRuntimeLease) (*ProbeWatchdogRuntime, error) { return r, nil }
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	err := svc.withRuntime(ctx, "22222222-2222-4222-8222-222222222222", func(ctx context.Context, _ domain.ProbeRuntimeLease, _ *ProbeWatchdogRuntime) error {
		waitWatchdogSignal(t, started)
		seconds.Store(21)
		<-ctx.Done()
		if leases.runtimeReleased.Load() != 0 {
			t.Error("parent released before action joined")
		}
		return ctx.Err()
	})
	if !errors.Is(err, ports.ErrConflict) || ctx.Err() != nil || leases.runtimeReleased.Load() != 1 {
		t.Fatal("watchdog stall retained parent lease", err)
	}
	select {
	case <-joined:
	default:
		t.Fatal("parent released before watchdog storage joined")
	}
}
