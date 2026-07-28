package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- Test doubles --------------------------------------------------------

// mockMonitorRepo is an in-memory MonitorRepository for tests.
type mockMonitorRepo struct {
	mu       sync.Mutex
	monitors []*domain.Monitor
}

func newMockMonitorRepo(monitors ...*domain.Monitor) *mockMonitorRepo {
	return &mockMonitorRepo{monitors: monitors}
}

func (r *mockMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.monitors = append(r.monitors, m)
	return nil
}

func (r *mockMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.monitors {
		if m.ID == id {
			return m, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *mockMonitorRepo) GetByPushToken(_ context.Context, token string) (*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.monitors {
		if m.PushToken == token {
			return m, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (r *mockMonitorRepo) List(_ context.Context, filter ports.MonitorFilter) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*domain.Monitor, 0, len(r.monitors))
	for _, m := range r.monitors {
		if filter.UserID > 0 && m.UserID != filter.UserID {
			continue
		}
		if filter.Type != "" && m.Type != filter.Type {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *mockMonitorRepo) ListActive(_ context.Context) ([]*domain.Monitor, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Monitor
	for _, m := range r.monitors {
		if m.Active {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *mockMonitorRepo) Update(_ context.Context, m *domain.Monitor) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, existing := range r.monitors {
		if existing.ID == m.ID {
			r.monitors[i] = m
			return nil
		}
	}
	return ports.ErrNotFound
}

func (r *mockMonitorRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, m := range r.monitors {
		if m.ID == id {
			r.monitors = append(r.monitors[:i], r.monitors[i+1:]...)
			return nil
		}
	}
	return ports.ErrNotFound
}

func (r *mockMonitorRepo) ClaimBatch(_ context.Context, _ string, _ int, _ time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *mockMonitorRepo) RefreshLease(_ context.Context, _ string) (int64, error)  { return 0, nil }
func (r *mockMonitorRepo) ReleaseLeases(_ context.Context, _ string) (int64, error) { return 0, nil }

// mockHeartbeatRepo records saved heartbeats for verification.
type mockHeartbeatRepo struct {
	mu          sync.Mutex
	heartbeats  []*domain.Heartbeat
	latestByMID map[int64]*domain.Heartbeat
}

func newMockHeartbeatRepo() *mockHeartbeatRepo {
	return &mockHeartbeatRepo{
		heartbeats:  make([]*domain.Heartbeat, 0),
		latestByMID: make(map[int64]*domain.Heartbeat),
	}
}

func (r *mockHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	h.ID = int64(len(r.heartbeats) + 1)
	if h.Time.IsZero() {
		h.Time = time.Now().UTC()
	}
	r.heartbeats = append(r.heartbeats, h)
	r.latestByMID[h.MonitorID] = h
	return nil
}

func (r *mockHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.latestByMID[monitorID]; ok {
		return h, nil
	}
	return nil, ports.ErrNotFound
}

func (r *mockHeartbeatRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Heartbeat
	for _, h := range r.heartbeats {
		if h.MonitorID == monitorID && !h.Time.Before(from) && !h.Time.After(to) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (r *mockHeartbeatRepo) SaveAggregate1m(_ context.Context, agg *ports.Aggregate1m) error {
	return nil
}
func (r *mockHeartbeatRepo) SaveAggregate1h(_ context.Context, agg *ports.Aggregate1h) error {
	return nil
}
func (r *mockHeartbeatRepo) SaveAggregate1d(_ context.Context, agg *ports.Aggregate1d) error {
	return nil
}
func (r *mockHeartbeatRepo) GetAggregate1m(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *mockHeartbeatRepo) GetAggregate1h(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *mockHeartbeatRepo) GetAggregate1d(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

func (r *mockHeartbeatRepo) DeleteByMonitor(_ context.Context, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := r.heartbeats[:0]
	for _, h := range r.heartbeats {
		if h.MonitorID != monitorID {
			filtered = append(filtered, h)
		}
	}
	r.heartbeats = filtered
	delete(r.latestByMID, monitorID)
	return nil
}

func (r *mockHeartbeatRepo) DeleteOlderThan(_ context.Context, _ time.Time) error { return nil }

func (r *mockHeartbeatRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.heartbeats)
}

// mockBus records published events.
type mockBus struct {
	mu     sync.Mutex
	events []ports.Event
}

func newMockBus() *mockBus {
	return &mockBus{events: make([]ports.Event, 0)}
}

func (b *mockBus) Publish(_ context.Context, event ports.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *mockBus) Subscribe(eventType string) <-chan ports.Event {
	ch := make(chan ports.Event, 100)
	return ch
}

func (b *mockBus) Close() {}

// mockChecker returns a fixed result.
type mockChecker struct {
	result ports.CheckResult
	err    error
}

func (c *mockChecker) Type() string { return "mock" }

func (c *mockChecker) Validate(config map[string]any) error { return nil }

func (c *mockChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	return c.result, c.err
}

// --- Tests ---------------------------------------------------------------

func TestScheduler_Run_ExecutesChecks(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       1,
			Name:     "test-monitor",
			Type:     "http",
			Active:   true,
			Interval: 1, // check every 1 second
			Timeout:  5,
			Config:   map[string]any{"url": "https://example.com"},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	// Create a checker that returns UP.
	checker := &mockChecker{
		result: ports.CheckResult{
			Status:    domain.StatusUp,
			LatencyMs: 10,
			Message:   "OK",
		},
	}

	checkerFn := func(t string) (ports.Checker, bool) {
		if t == "http" {
			return checker, true
		}
		return nil, false
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	// Run scheduler for 2.5 seconds.
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	err := sched.Run(ctx)
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}

	// At least one heartbeat should have been recorded.
	count := heartbeatRepo.count()
	if count == 0 {
		t.Error("expected at least 1 heartbeat to be recorded, got 0")
	}
}

func TestScheduler_Run_SkipsInactiveMonitors(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       1,
			Name:     "inactive-monitor",
			Type:     "http",
			Active:   false,
			Interval: 1,
			Timeout:  5,
			Config:   map[string]any{"url": "https://example.com"},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	checkerFn := func(t string) (ports.Checker, bool) {
		return &mockChecker{
			result: ports.CheckResult{Status: domain.StatusUp},
		}, true
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_ = sched.Run(ctx)

	// No heartbeats should be recorded for inactive monitors.
	count := heartbeatRepo.count()
	if count != 0 {
		t.Errorf("expected 0 heartbeats for inactive monitor, got %d", count)
	}
}

func TestScheduler_Run_HandlesPanic(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       1,
			Name:     "panicking-monitor",
			Type:     "panic",
			Active:   true,
			Interval: 1,
			Timeout:  5,
			Config:   map[string]any{},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	// Checker that panics.
	panickingChecker := &panicChecker{}

	checkerFn := func(t string) (ports.Checker, bool) {
		if t == "panic" {
			return panickingChecker, true
		}
		return nil, false
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	// Should not panic.
	err := sched.Run(ctx)
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
}

func TestScheduler_Run_WithUpsideDown(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:         1,
			Name:       "upside-down-monitor",
			Type:       "http",
			Active:     true,
			Interval:   1,
			Timeout:    5,
			UpsideDown: true,
			Config:     map[string]any{"url": "https://example.com"},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	checker := &mockChecker{
		result: ports.CheckResult{Status: domain.StatusUp},
	}

	checkerFn := func(t string) (ports.Checker, bool) {
		if t == "http" {
			return checker, true
		}
		return nil, false
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	_ = sched.Run(ctx)

	// Heartbeats should have been recorded with flipped status (DOWN instead of UP).
	count := heartbeatRepo.count()
	if count == 0 {
		t.Fatal("expected at least 1 heartbeat, got 0")
	}

	latest, err := heartbeatRepo.GetLatest(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if latest.Status != domain.StatusDown {
		t.Errorf("with UpsideDown=true and check returning UP, expected heartbeat status DOWN, got %v", latest.Status)
	}
}

func TestScheduler_Run_RecordsAfterCheckContextDeadline(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       1,
			Name:     "slow-http",
			Type:     "http",
			Active:   true,
			Interval: 1,
			Timeout:  1,
			Config:   map[string]any{"url": "https://example.com"},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	slowChecker := &slowDeadlineChecker{}

	checkerFn := func(t string) (ports.Checker, bool) {
		if t == "http" {
			return slowChecker, true
		}
		return nil, false
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()

	_ = sched.Run(ctx)

	if heartbeatRepo.count() == 0 {
		t.Fatal("expected heartbeat recorded even when check context deadline exceeded")
	}
}

func TestScheduler_Run_SkipsUnknownChecker(t *testing.T) {
	monitorRepo := newMockMonitorRepo(
		&domain.Monitor{
			ID:       1,
			Name:     "unknown-type",
			Type:     "nonexistent",
			Active:   true,
			Interval: 1,
			Timeout:  5,
			Config:   map[string]any{},
		},
	)

	heartbeatRepo := newMockHeartbeatRepo()
	bus := newMockBus()
	heartbeatSvc := services.NewHeartbeatService(heartbeatRepo, bus)

	checkerFn := func(t string) (ports.Checker, bool) {
		return nil, false
	}

	sched := NewLocalScheduler(
		monitorRepo,
		heartbeatRepo,
		checkerFn,
		heartbeatSvc,
		nil,
		slog.New(slog.DiscardHandler),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	_ = sched.Run(ctx)

	// No heartbeats should be recorded.
	count := heartbeatRepo.count()
	if count != 0 {
		t.Errorf("expected 0 heartbeats for unknown checker type, got %d", count)
	}
}

// TestCheckConfigForMonitor_TLSIgnore asserts the scheduler plumbs
// Monitor.TLSIgnore into the checker config map the same way it plumbs
// accepted_statuscodes — this is the link that made the flag a dead surface
// when it was missing.
func TestCheckConfigForMonitor_TLSIgnore(t *testing.T) {
	withFlag := &domain.Monitor{
		ID:        1,
		Type:      "http",
		Config:    map[string]any{"url": "https://example.com"},
		TLSIgnore: true,
	}
	cfg := checkConfigForMonitor(withFlag)
	if v, ok := cfg["tls_ignore"].(bool); !ok || !v {
		t.Errorf("tls_ignore in checker config = %v; want true", cfg["tls_ignore"])
	}

	withoutFlag := &domain.Monitor{
		ID:     2,
		Type:   "http",
		Config: map[string]any{"url": "https://example.com"},
	}
	cfg = checkConfigForMonitor(withoutFlag)
	if _, present := cfg["tls_ignore"]; present {
		t.Errorf("tls_ignore should be absent when Monitor.TLSIgnore is false, got %v", cfg["tls_ignore"])
	}
}

// --- Panic checker ------------------------------------------------------

// slowDeadlineChecker waits for ctx cancellation then returns DOWN (simulates timeout).
type slowDeadlineChecker struct{}

func (c *slowDeadlineChecker) Type() string                         { return "http" }
func (c *slowDeadlineChecker) Validate(config map[string]any) error { return nil }
func (c *slowDeadlineChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	<-ctx.Done()
	return ports.CheckResult{Status: domain.StatusDown, Message: ctx.Err().Error()}, ctx.Err()
}

type panicChecker struct{}

func (c *panicChecker) Type() string                         { return "panic" }
func (c *panicChecker) Validate(config map[string]any) error { return nil }
func (c *panicChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	panic("intentional panic in checker")
}
