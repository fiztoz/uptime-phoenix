package services

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Disposable fixture strings for push-lookup tests; never real credentials.
const (
	pushTokFixtureA = "fixture-push-a"
	pushTokFixtureB = "fixture-push-b"
)

// pushTokMonitorRepo is a faithful monitor store for push-lookup tests: it
// persists the dedicated lookup column on create AND update, and the lookup
// method queries only that column like both SQL adapters do. A fake that
// ignores Update would hide a desynchronized lookup field (issue #66).
type pushTokMonitorRepo struct {
	byID   map[int64]*domain.Monitor
	byLook map[string]int64
	nextID int64
}

func newPushTokMonitorRepo() *pushTokMonitorRepo {
	return &pushTokMonitorRepo{
		byID:   map[int64]*domain.Monitor{},
		byLook: map[string]int64{},
	}
}

func (r *pushTokMonitorRepo) reindex(m *domain.Monitor) {
	for look, id := range r.byLook {
		if id == m.ID && look != m.PushToken {
			delete(r.byLook, look)
		}
	}
	if m.PushToken != "" {
		r.byLook[m.PushToken] = m.ID
	}
}

func (r *pushTokMonitorRepo) Create(_ context.Context, m *domain.Monitor) error {
	r.nextID++
	m.ID = r.nextID
	cp := *m
	r.byID[m.ID] = &cp
	r.reindex(&cp)
	return nil
}

func (r *pushTokMonitorRepo) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	m, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *m
	return &cp, nil
}

func (r *pushTokMonitorRepo) GetByPushToken(_ context.Context, lookup string) (*domain.Monitor, error) {
	if lookup == "" {
		return nil, ports.ErrNotFound
	}
	id, ok := r.byLook[lookup]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *r.byID[id]
	return &cp, nil
}

func (r *pushTokMonitorRepo) Update(_ context.Context, m *domain.Monitor) error {
	if _, ok := r.byID[m.ID]; !ok {
		return ports.ErrNotFound
	}
	cp := *m
	r.byID[m.ID] = &cp
	r.reindex(&cp)
	return nil
}

func (r *pushTokMonitorRepo) List(context.Context, ports.MonitorFilter) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *pushTokMonitorRepo) ListActive(context.Context) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *pushTokMonitorRepo) Delete(_ context.Context, id int64) error {
	delete(r.byID, id)
	return nil
}
func (r *pushTokMonitorRepo) ClaimBatch(context.Context, string, int, time.Duration) ([]*domain.Monitor, error) {
	return nil, nil
}
func (r *pushTokMonitorRepo) RefreshLease(context.Context, string, time.Duration) (int64, error) {
	return 0, nil
}
func (r *pushTokMonitorRepo) ReleaseLeases(context.Context, string) (int64, error) {
	return 0, nil
}

func pushTokConfig(value string) map[string]any {
	cfg := map[string]any{}
	cfg["push_token"] = value
	return cfg
}

func pushTokMonitor(monitorType string, cfg map[string]any) *domain.Monitor {
	return &domain.Monitor{
		UserID: 1, Name: "Push", Type: monitorType, Active: true, Interval: 60,
		Config: cfg,
	}
}

// TestMonitorService_PushLookupSyncOnCreate asserts the shared service
// boundary fills the dedicated lookup column from config.push_token on
// create, so every creation path persists a resolvable value (issue #66).
func TestMonitorService_PushLookupSyncOnCreate(t *testing.T) {
	repo := newPushTokMonitorRepo()
	svc := NewMonitorService(repo, newFakeBus())
	m := pushTokMonitor("push", pushTokConfig(pushTokFixtureA))
	if err := svc.Create(context.Background(), m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.PushToken != pushTokFixtureA {
		t.Fatalf("PushToken = %q, want %q", m.PushToken, pushTokFixtureA)
	}
	got, err := svc.GetByPushToken(context.Background(), pushTokFixtureA)
	if err != nil || got.ID != m.ID {
		t.Fatalf("lookup after create: %v id=%v", err, got)
	}
}

// TestMonitorService_PushLookupSyncOnUpdate asserts an update keeps the
// lookup column aligned with the config value: explicit changes rotate the
// lookup and a redacted value preserves it without erasing the config.
func TestMonitorService_PushLookupSyncOnUpdate(t *testing.T) {
	repo := newPushTokMonitorRepo()
	svc := NewMonitorService(repo, newFakeBus())
	m := pushTokMonitor("push", pushTokConfig(pushTokFixtureA))
	if err := svc.Create(context.Background(), m); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Explicit change rotates the lookup.
	m.Config = pushTokConfig(pushTokFixtureB)
	if err := svc.Update(context.Background(), m); err != nil {
		t.Fatalf("update: %v", err)
	}
	if m.PushToken != pushTokFixtureB {
		t.Fatalf("PushToken = %q, want %q", m.PushToken, pushTokFixtureB)
	}
	if got, err := svc.GetByPushToken(context.Background(), pushTokFixtureB); err != nil || got.ID != m.ID {
		t.Fatalf("new lookup: %v", err)
	}
	if got, err := svc.GetByPushToken(context.Background(), pushTokFixtureA); err == nil {
		t.Fatalf("old lookup still resolves to %d", got.ID)
	}

	// Redacted value preserves both the stored lookup and the config value.
	m.Config = pushTokConfig(ConfigSecretRedacted)
	if err := svc.Update(context.Background(), m); err != nil {
		t.Fatalf("redacted update: %v", err)
	}
	if m.PushToken != pushTokFixtureB {
		t.Fatalf("redacted update changed PushToken to %q", m.PushToken)
	}
	if got := m.Config["push_token"]; got != pushTokFixtureB {
		t.Fatalf("redacted update replaced config value: %v", got)
	}
}

// TestMonitorService_PushCreateGeneratesTokenWhenOmitted asserts every create
// shape that supplies no usable token comes up with a freshly generated lookup
// token instead of a dead empty one (issue #65: create must match the UI's
// "generated" contract). The redacted sentinel is never persisted and each
// create mints a distinct, resolvable token.
func TestMonitorService_PushCreateGeneratesTokenWhenOmitted(t *testing.T) {
	repo := newPushTokMonitorRepo()
	svc := NewMonitorService(repo, newFakeBus())
	seen := map[string]bool{}
	for name, cfg := range map[string]map[string]any{
		"nil config":        nil,
		"empty config":      {},
		"empty string":      pushTokConfig(""),
		"redacted sentinel": pushTokConfig(ConfigSecretRedacted),
	} {
		m := pushTokMonitor("push", cfg)
		if err := svc.Create(context.Background(), m); err != nil {
			t.Fatalf("%s: create: %v", name, err)
		}
		if m.PushToken == "" || m.PushToken == ConfigSecretRedacted {
			t.Fatalf("%s: created PushToken = %q, want a generated token", name, m.PushToken)
		}
		if seen[m.PushToken] {
			t.Fatalf("%s: generated token was reused across creates", name)
		}
		seen[m.PushToken] = true
		if got := m.Config["push_token"]; got != m.PushToken {
			t.Fatalf("%s: config token = %v, want %q", name, got, m.PushToken)
		}
		if got, err := svc.GetByPushToken(context.Background(), m.PushToken); err != nil || got.ID != m.ID {
			t.Fatalf("%s: lookup by generated token: %v id=%v", name, err, got)
		}
	}
}

// pushTokAssignWriter persists the monitor like the production atomic
// create-with-assignments write and reports the fresh assignment set.
type pushTokAssignWriter struct{ repo *pushTokMonitorRepo }

func (w pushTokAssignWriter) CreateMonitorWithAssignments(ctx context.Context, m *domain.Monitor, _ []string, _ domain.HealthPolicy, _ []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	if err := w.repo.Create(ctx, m); err != nil {
		return nil, err
	}
	return &domain.MonitorProbeAssignments{MonitorID: m.ID}, nil
}

func (pushTokAssignWriter) ReplaceWithBindings(context.Context, int64, int64, []string, domain.HealthPolicy, []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	return nil, ports.ErrNotFound
}

// pushTokLocalRegistry answers the local-probe lookup that
// ValidateDesiredAssignments performs; the other port methods are unreachable
// in these tests.
type pushTokLocalRegistry struct{ ports.ProbeRegistryRepository }

func (pushTokLocalRegistry) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	if id != domain.LocalProbeID {
		return nil, ports.ErrNotFound
	}
	return &domain.Probe{ID: id, Key: id, Enabled: true}, nil
}

// pushTokCaps refuses remote execution: these tests create local-only sets, so
// a non-local member would fail loudly instead of being silently accepted.
type pushTokCaps struct{}

func (pushTokCaps) RemoteCapable(string) bool { return false }
func (pushTokCaps) ProxyCapable(string) bool  { return false }

// TestMonitorService_CreateWithAssignments_GeneratesPushToken asserts the
// atomic create-with-assignments path shares the create-time token contract
// (issue #65: every supported create path ships a working ingest token).
func TestMonitorService_CreateWithAssignments_GeneratesPushToken(t *testing.T) {
	repo := newPushTokMonitorRepo()
	svc := NewMonitorService(repo, newFakeBus())
	svc.SetAssignmentProvisioning(pushTokAssignWriter{repo}, pushTokLocalRegistry{}, pushTokCaps{}, FleetActivationGate{})
	m := pushTokMonitor("push", nil)
	initial := InitialAssignments{ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown}
	if err := svc.CreateWithAssignments(context.Background(), m, initial); err != nil {
		t.Fatalf("create with assignments: %v", err)
	}
	if m.PushToken == "" || m.PushToken == ConfigSecretRedacted {
		t.Fatalf("created PushToken = %q, want a generated token", m.PushToken)
	}
	if got := m.Config["push_token"]; got != m.PushToken {
		t.Fatalf("config token = %v, want %q", got, m.PushToken)
	}
	if got, err := svc.GetByPushToken(context.Background(), m.PushToken); err != nil || got.ID != m.ID {
		t.Fatalf("lookup by generated token: %v id=%v", err, got)
	}
}

// TestMonitorService_PushLookupLeavesOtherTypesAlone asserts non-push
// monitors never get their lookup field rewritten from config.
func TestMonitorService_PushLookupLeavesOtherTypesAlone(t *testing.T) {
	repo := newPushTokMonitorRepo()
	svc := NewMonitorService(repo, newFakeBus())
	cfg := map[string]any{"url": "https://example.com"}
	cfg["push_token"] = pushTokFixtureA
	m := pushTokMonitor("http", cfg)
	if err := svc.Create(context.Background(), m); err != nil {
		t.Fatalf("create: %v", err)
	}
	if m.PushToken != "" {
		t.Fatalf("PushToken = %q, want untouched for non-push monitors", m.PushToken)
	}
	if got, err := svc.GetByPushToken(context.Background(), pushTokFixtureA); err == nil {
		t.Fatalf("non-push lookup resolved to %d", got.ID)
	}
}
