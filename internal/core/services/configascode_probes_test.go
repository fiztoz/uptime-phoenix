package services_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// --- probe fakes -----------------------------------------------------------

type cfgProbeRegistry struct {
	mu     sync.Mutex
	byID   map[string]*domain.Probe
	byKey  map[string]*domain.Probe
	minted int
}

func newCfgProbeRegistry() *cfgProbeRegistry {
	return &cfgProbeRegistry{byID: map[string]*domain.Probe{}, byKey: map[string]*domain.Probe{}}
}

func (r *cfgProbeRegistry) Create(_ context.Context, p *domain.Probe) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p == nil || p.Kind != domain.ProbeKindRemote || p.Key == "" || p.Key == domain.LocalProbeID || p.Name == "" {
		return domain.ErrValidation
	}
	if p.ID == "" {
		r.minted++
		p.ID = "00000000-0000-4000-8000-00000000" + string(rune('a'+r.minted))
	}
	if _, ok := r.byID[p.ID]; ok {
		return ports.ErrConflict
	}
	if _, ok := r.byKey[p.Key]; ok {
		return ports.ErrConflict
	}
	cp := *p
	cp.Revision = 1
	cp.CreatedAt = time.Now().UTC()
	cp.UpdatedAt = cp.CreatedAt
	r.byID[cp.ID] = &cp
	r.byKey[cp.Key] = &cp
	*p = cp
	return nil
}

func (r *cfgProbeRegistry) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *cfgProbeRegistry) GetByKey(_ context.Context, key string) (*domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byKey[key]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *cfgProbeRegistry) List(context.Context) ([]domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.Probe, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, *p)
	}
	return out, nil
}

func (r *cfgProbeRegistry) Update(_ context.Context, p *domain.Probe, expectedRevision int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, ok := r.byID[p.ID]
	if !ok {
		return ports.ErrNotFound
	}
	if cur.Key != p.Key || cur.Kind != p.Kind {
		return domain.ErrValidation
	}
	if cur.Revision != expectedRevision {
		return ports.ErrConflict
	}
	cp := *p
	cp.Revision = expectedRevision + 1
	r.byID[cp.ID] = &cp
	r.byKey[cp.Key] = &cp
	*p = cp
	return nil
}

type cfgAssignmentRepo struct {
	mu         sync.Mutex
	registry   *cfgProbeRegistry
	sets       map[int64]*domain.MonitorProbeAssignments
	commitFail error
}

func newCfgAssignmentRepo(registry *cfgProbeRegistry) *cfgAssignmentRepo {
	return &cfgAssignmentRepo{registry: registry, sets: map[int64]*domain.MonitorProbeAssignments{}}
}

func (r *cfgAssignmentRepo) InitializeLocal(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if set, ok := r.sets[monitorID]; ok {
		cp := *set
		return &cp, nil
	}
	set := &domain.MonitorProbeAssignments{
		MonitorID: monitorID, Revision: 1, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Generation: 1}},
	}
	r.sets[monitorID] = set
	cp := *set
	return &cp, nil
}

func (r *cfgAssignmentRepo) GetByMonitorID(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set, ok := r.sets[monitorID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *set
	cp.Assignments = append([]domain.ProbeAssignment{}, set.Assignments...)
	return &cp, nil
}

func (r *cfgAssignmentRepo) Replace(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	return r.commit(ctx, monitorID, expectedRevision, probeIDs, policy, nil, true)
}

func (r *cfgAssignmentRepo) Restore(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	return r.commit(ctx, monitorID, expectedRevision, probeIDs, policy, bindings, false)
}

func (r *cfgAssignmentRepo) commit(_ context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding, requireEnabled bool) (*domain.MonitorProbeAssignments, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.commitFail != nil {
		return nil, r.commitFail
	}
	if len(probeIDs) == 0 || (policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return nil, domain.ErrValidation
	}
	current, ok := r.sets[monitorID]
	if !ok {
		if expectedRevision != 1 {
			return nil, ports.ErrNotFound
		}
		current = &domain.MonitorProbeAssignments{
			MonitorID: monitorID, Revision: 1, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Generation: 1}},
		}
		r.sets[monitorID] = current
	}
	if current.Revision != expectedRevision {
		return nil, ports.ErrConflict
	}
	bindingFor := map[string]domain.ProbeResourceBinding{}
	for _, b := range bindings {
		bindingFor[b.ProbeID] = b.ProbeResourceBinding
	}
	members := make([]domain.ProbeAssignment, 0, len(probeIDs))
	for _, id := range probeIDs {
		if id != domain.LocalProbeID {
			p, err := r.registry.GetByID(context.Background(), id)
			if err != nil {
				return nil, domain.ErrValidation
			}
			if requireEnabled && !p.Enabled {
				return nil, domain.ErrValidation
			}
		}
		a := domain.ProbeAssignment{MonitorID: monitorID, ProbeID: id, Generation: 1}
		if b, ok := bindingFor[id]; ok {
			a.ResourceBinding = &b
		}
		members = append(members, a)
	}
	set := &domain.MonitorProbeAssignments{MonitorID: monitorID, Revision: expectedRevision + 1, HealthPolicy: policy, Assignments: members}
	r.sets[monitorID] = set
	cp := *set
	return &cp, nil
}

func (r *cfgAssignmentRepo) ExecutableByLocal(_ context.Context, monitorIDs []int64) (map[int64]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]int64{}
	for _, id := range monitorIDs {
		if set, ok := r.sets[id]; ok {
			for _, a := range set.Assignments {
				if a.ProbeID == domain.LocalProbeID {
					out[id] = a.Generation
				}
			}
		}
	}
	return out, nil
}

func (r *cfgAssignmentRepo) ListHistory(context.Context, int64, time.Time, time.Time) ([]domain.AssignmentInterval, error) {
	return nil, nil
}

// --- harness ---------------------------------------------------------------

func newConfigProbeSvc(t *testing.T) (
	*services.ConfigService,
	*cfgProbeRegistry,
	*cfgAssignmentRepo,
	*cfgMonRepo,
) {
	t.Helper()
	svc, _, _, _, mons := newConfigSvc(t)
	registry := newCfgProbeRegistry()
	assignments := newCfgAssignmentRepo(registry)
	svc.SetProbeRegistry(registry)
	svc.SetProbeAssignments(assignments)
	svc.SetFleetActivationGate(services.NewFleetActivationGate(&configFleetReadiness{}, time.Minute))
	return svc, registry, assignments, mons
}

func probeDoc() *services.ConfigDocument {
	active := true
	return &services.ConfigDocument{
		APIVersion: services.ConfigAPIVersion,
		Kind:       services.ConfigKind,
		Spec: services.ConfigSpec{
			Probes: []services.ConfigProbe{
				{Key: "us-east", Name: "US East", Location: "ashburn"},
			},
			Monitors: []services.ConfigMonitor{
				{
					Key: "api-health", Name: "API Health", Type: "http", Active: &active,
					Interval: 60, Config: map[string]any{"url": "https://api.example.com/health"},
					ProbeAssignments: []services.ConfigMonitorProbeAssignment{
						{Probe: "local"}, {Probe: "us-east"},
					},
					HealthPolicy: "all_down",
				},
			},
		},
	}
}

// --- tests -----------------------------------------------------------------

func TestConfigValidate_ProbeDeclarationsAndAssignmentRefs(t *testing.T) {
	svc, _, _, _ := newConfigProbeSvc(t)
	ctx := context.Background()

	bad := probeDoc()
	bad.Spec.Probes[0].Key = "Local"
	bad.Spec.Monitors[0].ProbeAssignments = []services.ConfigMonitorProbeAssignment{{Probe: "local"}}
	if errs := svc.Validate(ctx, bad); len(errs) == 0 || !strings.Contains(strings.Join(errs, "; "), "invalid key") {
		t.Fatalf("reserved/invalid probe key accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].ProbeAssignments = append(bad.Spec.Monitors[0].ProbeAssignments, services.ConfigMonitorProbeAssignment{Probe: "us-east"})
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "duplicate probe key") {
		t.Fatalf("duplicate member accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].ProbeAssignments = []services.ConfigMonitorProbeAssignment{{Probe: "eu-west"}}
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "unknown probe key") {
		t.Fatalf("unknown probe ref accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].HealthPolicy = "some_down"
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "unsupported health_policy") {
		t.Fatalf("invalid policy accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].Type = "push"
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "push monitors cannot run remotely") {
		t.Fatalf("remote push assignment accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].Type = "docker"
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "requires a resource binding") {
		t.Fatalf("docker assignment without binding accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].ProbeAssignments = []services.ConfigMonitorProbeAssignment{
		{Probe: "local"}, {Probe: "us-east", BindingKey: "docker-main", BindingKind: "docker_socket"},
	}
	if errs := svc.Validate(ctx, bad); !containsSubstring(errs, "only valid for docker monitors") {
		t.Fatalf("binding on non-docker monitor accepted: %v", errs)
	}

	bad = probeDoc()
	bad.Spec.Monitors[0].Type = "docker"
	bad.Spec.Monitors[0].ProbeAssignments = []services.ConfigMonitorProbeAssignment{
		{Probe: "us-east", BindingKey: "docker-main", BindingKind: "docker_socket"},
	}
	if errs := svc.Validate(ctx, bad); len(errs) != 0 {
		t.Fatalf("valid docker declaration rejected: %v", errs)
	}
}

func TestConfigApply_ProbesAndAssignments_Idempotent(t *testing.T) {
	svc, registry, assignments, mons := newConfigProbeSvc(t)
	ctx := context.Background()
	doc := probeDoc()

	res, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Creates < 3 { // probe + monitor + probe_assignment
		t.Fatalf("creates=%d applied=%+v", res.Creates, res.Applied)
	}
	p, err := registry.GetByKey(ctx, "us-east")
	if err != nil || !p.Enabled || p.Name != "US East" {
		t.Fatalf("declared probe not created as declared: %+v %v", p, err)
	}
	if len(mons.byID) != 1 {
		t.Fatalf("monitors=%d", len(mons.byID))
	}
	var monitorID int64
	for id := range mons.byID {
		monitorID = id
	}
	set, err := assignments.GetByMonitorID(ctx, monitorID)
	if err != nil {
		t.Fatalf("assignment set: %v", err)
	}
	if set.HealthPolicy != domain.HealthPolicyAllDown || len(set.Assignments) != 2 {
		t.Fatalf("assignment set: %+v", set)
	}
	allowed, err := assignments.ExecutableByLocal(ctx, []int64{monitorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allowed[monitorID]; !ok {
		t.Fatal("declared set keeps the local member but lost local execution")
	}

	second, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if second.Creates != 0 || second.Updates != 0 || second.Deletes != 0 {
		t.Fatalf("second apply not idempotent: %+v", second)
	}

	// Export of the applied state must re-apply with zero drift.
	exported, err := svc.Export(ctx, 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported.Spec.Probes) != 1 || exported.Spec.Probes[0].Key != "us-east" || exported.Spec.Probes[0].Enabled == nil {
		t.Fatalf("exported probes: %+v", exported.Spec.Probes)
	}
	if len(exported.Spec.Monitors) != 1 || len(exported.Spec.Monitors[0].ProbeAssignments) != 2 {
		t.Fatalf("exported monitors: %+v", exported.Spec.Monitors)
	}
	if exported.Spec.Monitors[0].HealthPolicy != string(domain.HealthPolicyAllDown) {
		t.Fatalf("exported policy: %q", exported.Spec.Monitors[0].HealthPolicy)
	}
	third, err := svc.Apply(ctx, 1, exported, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("apply exported: %v", err)
	}
	if third.Creates != 0 || third.Updates != 0 || third.Deletes != 0 {
		t.Fatalf("exported document is not a fixed point: %+v", third)
	}
}

func TestConfigPlan_RejectsAssignmentToDisabledProbe(t *testing.T) {
	svc, registry, assignments, mons := newConfigProbeSvc(t)
	ctx := context.Background()

	t.Run("declared disabled", func(t *testing.T) {
		doc := probeDoc()
		off := false
		doc.Spec.Probes[0].Enabled = &off
		plan, err := svc.Plan(ctx, 1, doc, services.ConfigApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Valid || !containsSubstring(plan.Errors, "is disabled") {
			t.Fatalf("plan accepted a disabled member: %+v", plan)
		}
		if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err == nil {
			t.Fatal("apply must refuse a document that assigns a disabled probe")
		}
		if len(registry.byID) != 0 || len(mons.byID) != 0 || len(assignments.sets) != 0 {
			t.Fatal("refused document still wrote state")
		}
	})

	t.Run("existing registration stays disabled", func(t *testing.T) {
		doc := probeDoc()
		if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		// Disable the registration, then re-declare it without `enabled`
		// (omitted keeps the current state) while assigning it.
		p, err := registry.GetByKey(ctx, "us-east")
		if err != nil {
			t.Fatal(err)
		}
		p.Enabled = false
		if err := registry.Update(ctx, p, p.Revision); err != nil {
			t.Fatal(err)
		}
		doc2 := probeDoc()
		doc2.Spec.Probes[0].Enabled = nil
		plan, err := svc.Plan(ctx, 1, doc2, services.ConfigApplyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Valid || !containsSubstring(plan.Errors, "is disabled") {
			t.Fatalf("plan assigned a disabled registration: %+v", plan)
		}
	})
}

func TestConfigApply_PruneNeverDeletesProbeRegistrations(t *testing.T) {
	svc, registry, _, _ := newConfigProbeSvc(t)
	ctx := context.Background()
	doc := probeDoc()
	doc.Spec.Probes = append(doc.Spec.Probes, services.ConfigProbe{Key: "eu-west", Name: "EU West", Location: "dublin"})
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}

	trimmed := probeDoc()
	if _, err := svc.Apply(ctx, 1, trimmed, services.ConfigApplyOptions{Prune: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.GetByKey(ctx, "eu-west"); err != nil {
		t.Fatalf("prune deleted a probe registration: %v", err)
	}
	if _, err := registry.GetByKey(ctx, "us-east"); err != nil {
		t.Fatalf("declared probe disappeared: %v", err)
	}
}

func TestConfigApply_UpdateProbeMetadataAndEnabled(t *testing.T) {
	svc, registry, _, _ := newConfigProbeSvc(t)
	ctx := context.Background()
	doc := probeDoc()
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	updated := probeDoc()
	updated.Spec.Probes[0].Location = "frankfurt"
	off := false
	updated.Spec.Probes[0].Enabled = &off
	// Drop the assignment so disabling is not refused by the plan.
	updated.Spec.Monitors[0].ProbeAssignments = nil
	res, err := svc.Apply(ctx, 1, updated, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Updates < 1 {
		t.Fatalf("expected probe update: %+v", res)
	}
	p, err := registry.GetByKey(ctx, "us-east")
	if err != nil || p.Location != "frankfurt" || p.Enabled {
		t.Fatalf("probe not updated: %+v %v", p, err)
	}
}

func containsSubstring(haystack []string, needle string) bool {
	for _, s := range haystack {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// Config writes are live declarations, including re-enabling an existing identity.
type configFleetReadiness struct {
	unaware []string
	err     error
}

func (*configFleetReadiness) DeclareWorker(context.Context, string, int, time.Duration) error {
	return nil
}
func (r *configFleetReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	return r.unaware, r.err
}

func TestConfigApplyFleetGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate services.FleetActivationGate
		want error
	}{
		{"unaware", services.NewFleetActivationGate(&configFleetReadiness{unaware: []string{"old-worker"}}, time.Minute), services.ErrFleetNotAssignmentAware},
		{"unavailable", services.NewFleetActivationGate(&configFleetReadiness{err: errors.New("db down")}, time.Minute), services.ErrFleetReadinessUnavailable},
		{"unwired", services.FleetActivationGate{}, services.ErrFleetReadinessUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, registry, assignments, mons := newConfigProbeSvc(t)
			// Reproduce the live-identity path, not only a new disabled registration.
			p := &domain.Probe{ID: "11111111-1111-4111-8111-111111111111", Key: "us-east", Name: "original", Kind: domain.ProbeKindRemote, Enabled: true}
			if err := registry.Create(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			svc.SetFleetActivationGate(tc.gate)
			_, err := svc.Apply(t.Context(), 1, probeDoc(), services.ConfigApplyOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if len(mons.byID) != 0 || len(assignments.sets) != 0 {
				t.Fatal("refused config applied monitor/assignment state")
			}
			stored, err := registry.GetByID(t.Context(), p.ID)
			if err != nil || stored.Name != "original" {
				t.Fatal("refused config changed registration")
			}
		})
	}
}

func TestConfigApplyUnchangedRemoteSetDoesNotActivate(t *testing.T) {
	svc, _, assignments, _ := newConfigProbeSvc(t)
	doc := probeDoc()
	if _, err := svc.Apply(t.Context(), 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var revision int64
	for _, set := range assignments.sets {
		revision = set.Revision
	}
	svc.SetFleetActivationGate(services.NewFleetActivationGate(&configFleetReadiness{unaware: []string{"old-worker"}}, time.Minute))
	result, err := svc.Apply(t.Context(), 1, doc, services.ConfigApplyOptions{})
	if err != nil {
		t.Fatalf("no-op apply gated: %v", err)
	}
	if result.Creates != 0 || result.Updates != 0 || result.Deletes != 0 {
		t.Fatalf("no-op mutated: %+v", result)
	}
	for _, set := range assignments.sets {
		if set.Revision != revision {
			t.Fatal("no-op changed assignment revision")
		}
	}
}
