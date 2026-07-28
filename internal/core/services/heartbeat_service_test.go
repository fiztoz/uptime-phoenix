package services

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- Test doubles --------------------------------------------------------

// fakeHeartbeatRepo is an in-memory HeartbeatRepository for tests.
type fakeHeartbeatRepo struct {
	mu               sync.Mutex
	heartbeats       []*domain.Heartbeat
	latest           map[int64]*domain.Heartbeat // monitorID -> latest
	errOnSave        error
	lastDeleteCutoff time.Time // last bound passed to DeleteOlderThan
}

func newFakeHeartbeatRepo() *fakeHeartbeatRepo {
	return &fakeHeartbeatRepo{
		heartbeats: make([]*domain.Heartbeat, 0),
		latest:     make(map[int64]*domain.Heartbeat),
	}
}

func (r *fakeHeartbeatRepo) Save(_ context.Context, h *domain.Heartbeat) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.errOnSave != nil {
		return r.errOnSave
	}
	h.ID = int64(len(r.heartbeats) + 1)
	r.heartbeats = append(r.heartbeats, h)
	r.latest[h.MonitorID] = h
	return nil
}

func (r *fakeHeartbeatRepo) GetLatest(_ context.Context, monitorID int64) (*domain.Heartbeat, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.latest[monitorID]; ok {
		return h, nil
	}
	return nil, errors.New("not found")
}

func (r *fakeHeartbeatRepo) ListByMonitor(_ context.Context, monitorID int64, from, to time.Time) ([]*domain.Heartbeat, error) {
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

func (r *fakeHeartbeatRepo) SaveAggregate1m(_ context.Context, agg *ports.Aggregate1m) error {
	return nil
}
func (r *fakeHeartbeatRepo) SaveAggregate1h(_ context.Context, agg *ports.Aggregate1h) error {
	return nil
}
func (r *fakeHeartbeatRepo) SaveAggregate1d(_ context.Context, agg *ports.Aggregate1d) error {
	return nil
}
func (r *fakeHeartbeatRepo) GetAggregate1m(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1m, error) {
	return nil, nil
}
func (r *fakeHeartbeatRepo) GetAggregate1h(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1h, error) {
	return nil, nil
}
func (r *fakeHeartbeatRepo) GetAggregate1d(_ context.Context, monitorID int64, from time.Time) ([]*ports.Aggregate1d, error) {
	return nil, nil
}

func (r *fakeHeartbeatRepo) DeleteByMonitor(_ context.Context, monitorID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := r.heartbeats[:0]
	for _, h := range r.heartbeats {
		if h.MonitorID != monitorID {
			filtered = append(filtered, h)
		}
	}
	r.heartbeats = filtered
	delete(r.latest, monitorID)
	return nil
}

// lastDeleteCutoff records the last bound passed to DeleteOlderThan so tests
// can assert Location() == time.UTC (in-memory comparisons alone cannot catch
// a local-zone bug — AGENTS.md rule 6).
func (r *fakeHeartbeatRepo) DeleteOlderThan(_ context.Context, before time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastDeleteCutoff = before
	filtered := r.heartbeats[:0]
	for _, h := range r.heartbeats {
		if !h.Time.Before(before) {
			filtered = append(filtered, h)
		} else if r.latest[h.MonitorID] == h {
			delete(r.latest, h.MonitorID)
		}
	}
	r.heartbeats = filtered
	return nil
}

// fakeBus is an in-memory EventBus that records published events.
type fakeBus struct {
	mu     sync.Mutex
	events []ports.Event
	subs   map[string][]chan ports.Event
}

func newFakeBus() *fakeBus {
	return &fakeBus{
		events: make([]ports.Event, 0),
		subs:   make(map[string][]chan ports.Event),
	}
}

func (b *fakeBus) Publish(_ context.Context, event ports.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, event)
	return nil
}

func (b *fakeBus) Subscribe(eventType string) <-chan ports.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan ports.Event, 100)
	b.subs[eventType] = append(b.subs[eventType], ch)
	return ch
}

func (b *fakeBus) Close() {}

func (b *fakeBus) publishedEvents() []ports.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ports.Event, len(b.events))
	copy(out, b.events)
	return out
}

// --- Tests ---------------------------------------------------------------

func TestHeartbeatService_Record_SavesHeartbeat(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}
	result := ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: 42,
		Message:   "OK",
	}

	err := svc.Record(context.Background(), monitor, result)
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	// Verify heartbeat was saved.
	latest, err := repo.GetLatest(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetLatest returned error: %v", err)
	}
	if latest == nil {
		t.Fatal("GetLatest returned nil")
	}
	if latest.Status != domain.StatusUp {
		t.Errorf("heartbeat status = %v; want UP", latest.Status)
	}
	if latest.Ping != 42 {
		t.Errorf("heartbeat ping = %d; want 42", latest.Ping)
	}
	if latest.Msg != "OK" {
		t.Errorf("heartbeat msg = %q; want OK", latest.Msg)
	}
	if latest.Time.IsZero() {
		t.Errorf("heartbeat time should be set")
	}
}

func TestHeartbeatService_Record_PublishesHeartbeatEvent(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}
	result := ports.CheckResult{Status: domain.StatusUp}

	err := svc.Record(context.Background(), monitor, result)
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	events := bus.publishedEvents()
	hasHeartbeatEvent := false
	for _, ev := range events {
		if ev.Type == "heartbeat" {
			hasHeartbeatEvent = true
			break
		}
	}
	if !hasHeartbeatEvent {
		t.Error("expected heartbeat event to be published")
	}
}

func TestHeartbeatService_Record_StatusTransitionUpToDown(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	// First check: UP
	err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp})
	if err != nil {
		t.Fatalf("first Record: %v", err)
	}

	// Clear events from first Record.
	bus.events = nil

	// Second check: DOWN — should trigger status.change.
	err = svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown})
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}

	events := bus.publishedEvents()
	hasStatusChange := false
	for _, ev := range events {
		if ev.Type == "status.change" {
			hasStatusChange = true
			payload, ok := ev.Payload.(map[string]any)
			if !ok {
				t.Fatal("status.change payload is not a map")
			}
			if payload["monitor_id"] != int64(1) {
				t.Errorf("status.change monitor_id = %v; want 1", payload["monitor_id"])
			}
			if payload["old_status"] != domain.StatusUp {
				t.Errorf("status.change old_status = %v; want UP", payload["old_status"])
			}
			if payload["new_status"] != domain.StatusDown {
				t.Errorf("status.change new_status = %v; want DOWN", payload["new_status"])
			}
			break
		}
	}
	if !hasStatusChange {
		t.Error("expected status.change event on UP→DOWN transition")
	}
}

func TestHeartbeatService_Record_NoTransitionOnSameStatus(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	// First check: UP.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("first Record: %v", err)
	}

	// Clear events.
	bus.events = nil

	// Second check: UP again — no transition.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("second Record: %v", err)
	}

	events := bus.publishedEvents()
	for _, ev := range events {
		if ev.Type == "status.change" {
			t.Error("status.change should not be published when status is unchanged")
		}
	}
}

func TestHeartbeatService_Record_DownCountIncrement(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	// First check: UP — DownCount should be 0.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	latest, _ := repo.GetLatest(context.Background(), 1)
	if latest.DownCount != 0 {
		t.Errorf("after UP, DownCount = %d; want 0", latest.DownCount)
	}

	// Second check: DOWN — DownCount should be 1.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
		t.Fatalf("second Record: %v", err)
	}
	latest, _ = repo.GetLatest(context.Background(), 1)
	if latest.DownCount != 1 {
		t.Errorf("after first DOWN, DownCount = %d; want 1", latest.DownCount)
	}

	// Third check: DOWN again — DownCount should be 2.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
		t.Fatalf("third Record: %v", err)
	}
	latest, _ = repo.GetLatest(context.Background(), 1)
	if latest.DownCount != 2 {
		t.Errorf("after second DOWN, DownCount = %d; want 2", latest.DownCount)
	}

	// Fourth check: UP — DownCount should reset to 0.
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("fourth Record: %v", err)
	}
	latest, _ = repo.GetLatest(context.Background(), 1)
	if latest.DownCount != 0 {
		t.Errorf("after UP after DOWN, DownCount = %d; want 0", latest.DownCount)
	}
}

func TestHeartbeatService_Record_ReturnsSaveError(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	repo.errOnSave = errors.New("db error")

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}
	result := ports.CheckResult{Status: domain.StatusUp}

	err := svc.Record(context.Background(), monitor, result)
	if err == nil {
		t.Fatal("expected error from Record, got nil")
	}
}

func TestHeartbeatService_Record_DoesNotFailOnBusPublish(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	// Record should succeed even if bus publish fails (best-effort).
	err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp})
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
}

func TestHeartbeatService_Record_FirstHeartbeatPublishesStatusChange(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	// First-ever heartbeat should publish status.change so the UI leaves "pending".
	err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp})
	if err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	events := bus.publishedEvents()
	var found bool
	for _, ev := range events {
		if ev.Type == "status.change" {
			found = true
			break
		}
	}
	if !found {
		t.Error("status.change should be published for the first heartbeat")
	}
}

// fakeDispatcher records the effective status of each heartbeat it receives.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []domain.Status
}

func (f *fakeDispatcher) OnHeartbeat(_ context.Context, _ *domain.Monitor, hb *domain.Heartbeat, _ *domain.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, hb.Status)
}

func TestHeartbeatService_Record_RetryConfirmPendingThenDown(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	// MaxRetries=2 → two PENDING checks before the monitor is confirmed DOWN.
	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http", MaxRetries: 2}

	want := []domain.Status{domain.StatusPending, domain.StatusPending, domain.StatusDown}
	for i, w := range want {
		if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
			t.Fatalf("Record %d: %v", i+1, err)
		}
		latest, _ := repo.GetLatest(context.Background(), 1)
		if latest.Status != w {
			t.Errorf("check %d: effective status = %v; want %v", i+1, latest.Status, w)
		}
		if latest.DownCount != i+1 {
			t.Errorf("check %d: DownCount = %d; want %d", i+1, latest.DownCount, i+1)
		}
	}

	// The confirmed DOWN (third check) must be the heartbeat marked important.
	latest, _ := repo.GetLatest(context.Background(), 1)
	if !latest.Important {
		t.Error("confirmed DOWN heartbeat should be marked important")
	}
}

func TestHeartbeatService_Record_NoRetryWhenMaxRetriesZero(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)

	// MaxRetries=0 → fail immediately, no PENDING state.
	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	latest, _ := repo.GetLatest(context.Background(), 1)
	if latest.Status != domain.StatusDown {
		t.Errorf("status = %v; want DOWN immediately when MaxRetries=0", latest.Status)
	}
}

func TestHeartbeatService_Record_InvokesDispatcher(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	disp := &fakeDispatcher{}
	svc := NewHeartbeatService(repo, bus)
	svc.SetDispatcher(disp)

	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusDown}); err != nil {
		t.Fatalf("second Record: %v", err)
	}

	if len(disp.calls) != 2 {
		t.Fatalf("dispatcher called %d times; want 2 (once per heartbeat)", len(disp.calls))
	}
	if disp.calls[0] != domain.StatusUp || disp.calls[1] != domain.StatusDown {
		t.Errorf("dispatched statuses = %v; want [UP DOWN]", disp.calls)
	}
}

func TestHeartbeatService_ClearHistory(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	svc := NewHeartbeatService(repo, bus)
	monitor := &domain.Monitor{ID: 1, Name: "test", Type: "http"}

	if err := svc.Record(context.Background(), monitor, ports.CheckResult{Status: domain.StatusUp}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := svc.ClearHistory(context.Background(), 1); err != nil {
		t.Fatalf("ClearHistory: %v", err)
	}
	_, err := repo.GetLatest(context.Background(), 1)
	if err == nil {
		t.Fatal("expected no heartbeats after clear")
	}
}

// DeleteOlderThan must force the cutoff to UTC before calling the repo.
// A local-zoned cutoff would delete rows up to the host offset *newer*
// than intended (AGENTS.md rule 6). We assert Location(), not only the
// instant — an in-memory fake that compares instants cannot catch zone bugs.
func TestHeartbeatService_DeleteOlderThan_CutoffIsUTC(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	svc := NewHeartbeatService(repo, newFakeBus())

	// Local +7 wall-clock, as on a Bangkok host.
	loc := time.FixedZone("UTC+7", 7*3600)
	localCutoff := time.Date(2025, 1, 1, 12, 0, 0, 0, loc)

	if err := svc.DeleteOlderThan(context.Background(), localCutoff); err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}

	got := repo.lastDeleteCutoff
	if got.IsZero() {
		t.Fatal("repo.DeleteOlderThan was never called")
	}
	if got.Location() != time.UTC {
		t.Errorf("cutoff Location() = %v; want time.UTC (local-zoned cutoffs destroy recent rows)", got.Location())
	}
	if !got.Equal(localCutoff.UTC()) {
		t.Errorf("cutoff = %v; want %v", got, localCutoff.UTC())
	}
}

func TestHeartbeatService_DeleteOlderThan_DeletesOldRows(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	svc := NewHeartbeatService(repo, newFakeBus())
	now := time.Now().UTC()
	old := &domain.Heartbeat{ID: 1, MonitorID: 1, Status: domain.StatusUp, Time: now.AddDate(0, 0, -200)}
	recent := &domain.Heartbeat{ID: 2, MonitorID: 1, Status: domain.StatusUp, Time: now.AddDate(0, 0, -1)}
	_ = repo.Save(context.Background(), old)
	_ = repo.Save(context.Background(), recent)

	cutoff := now.AddDate(0, 0, -180)
	if err := svc.DeleteOlderThan(context.Background(), cutoff); err != nil {
		t.Fatalf("DeleteOlderThan: %v", err)
	}

	remaining, err := repo.ListByMonitor(context.Background(), 1, now.AddDate(-1, 0, 0), now.Add(time.Hour))
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ID != recent.ID {
		t.Fatalf("remaining = %+v; want only recent heartbeat id=%d", remaining, recent.ID)
	}
}

func TestHeartbeatService_Record_PersistsTLSInfo(t *testing.T) {
	repo := newFakeHeartbeatRepo()
	bus := newFakeBus()
	tlsRepo := newStatsFakeTLSRepo()
	svc := NewHeartbeatService(repo, bus)
	svc.SetTLSInfoRepo(tlsRepo)

	monitor := &domain.Monitor{ID: 1, Name: "https-test", Type: "http"}
	exactExpiry := time.Date(2030, 7, 1, 12, 0, 0, 0, time.UTC)
	result := ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: 12,
		Message:   "200 - OK",
		Metadata: map[string]string{
			"tls_days_remaining": "30",
			"tls_issuer":         "Test CA",
			"tls_not_after":      exactExpiry.Format(time.RFC3339),
		},
	}

	if err := svc.Record(context.Background(), monitor, result); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}

	info, err := tlsRepo.GetByMonitorID(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetByMonitorID returned error: %v", err)
	}
	if info.DaysRemaining != 30 {
		t.Errorf("DaysRemaining = %d, want 30", info.DaysRemaining)
	}
	if info.Issuer != "Test CA" {
		t.Errorf("Issuer = %q, want Test CA", info.Issuer)
	}
	if !info.NotAfter.Equal(exactExpiry) {
		t.Errorf("NotAfter = %v, want exact %v (must not reconstruct from days)", info.NotAfter, exactExpiry)
	}
}
