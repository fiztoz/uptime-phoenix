package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- probe fakes -----------------------------------------------------------
// These mirror the probe store contracts closely enough to hold the service to
// its own promises: identity uniqueness, disabled restored identities, complete
// desired sets and local-execution gating. Store-level semantics (generations,
// tombstones, history) are verified against real engines in
// internal/adapters/repository.

type backupFakeProbeRegistry struct {
	mu     sync.Mutex
	byID   map[string]*domain.Probe
	byKey  map[string]*domain.Probe
	minted int
}

func newBackupFakeProbeRegistry() *backupFakeProbeRegistry {
	return &backupFakeProbeRegistry{byID: map[string]*domain.Probe{}, byKey: map[string]*domain.Probe{}}
}

func (r *backupFakeProbeRegistry) Create(_ context.Context, p *domain.Probe) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p == nil || p.Kind != domain.ProbeKindRemote || p.Key == "" || p.Key == domain.LocalProbeID || p.Name == "" {
		return domain.ErrValidation
	}
	if p.ID == "" {
		r.minted++
		p.ID = fmt.Sprintf("00000000-0000-4000-8000-%012d", r.minted)
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

func (r *backupFakeProbeRegistry) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *backupFakeProbeRegistry) GetByKey(_ context.Context, key string) (*domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.byKey[key]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (r *backupFakeProbeRegistry) List(context.Context) ([]domain.Probe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.Probe, 0, len(r.byID))
	for _, p := range r.byID {
		out = append(out, *p)
	}
	return out, nil
}

func (r *backupFakeProbeRegistry) Update(_ context.Context, p *domain.Probe, expectedRevision int64) error {
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

type backupFakeAssignmentRepo struct {
	mu         sync.Mutex
	registry   *backupFakeProbeRegistry
	sets       map[int64]*domain.MonitorProbeAssignments
	generation map[int64]map[string]int64
	restoreErr error
	restores   int
}

func newBackupFakeAssignmentRepo(registry *backupFakeProbeRegistry) *backupFakeAssignmentRepo {
	return &backupFakeAssignmentRepo{
		registry:   registry,
		sets:       map[int64]*domain.MonitorProbeAssignments{},
		generation: map[int64]map[string]int64{},
	}
}

func (r *backupFakeAssignmentRepo) InitializeLocal(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
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
	r.generation[monitorID] = map[string]int64{domain.LocalProbeID: 1}
	cp := *set
	return &cp, nil
}

func (r *backupFakeAssignmentRepo) GetByMonitorID(_ context.Context, monitorID int64) (*domain.MonitorProbeAssignments, error) {
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

func (r *backupFakeAssignmentRepo) Replace(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy) (*domain.MonitorProbeAssignments, error) {
	return r.commit(ctx, monitorID, expectedRevision, probeIDs, policy, nil, true)
}

func (r *backupFakeAssignmentRepo) Restore(ctx context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	return r.commit(ctx, monitorID, expectedRevision, probeIDs, policy, bindings, false)
}

func (r *backupFakeAssignmentRepo) commit(_ context.Context, monitorID, expectedRevision int64, probeIDs []string, policy domain.HealthPolicy, bindings []domain.ProbeAssignmentBinding, requireEnabled bool) (*domain.MonitorProbeAssignments, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.restores++
	if r.restoreErr != nil {
		return nil, r.restoreErr
	}
	if len(probeIDs) == 0 || (policy != domain.HealthPolicyAnyDown && policy != domain.HealthPolicyAllDown) {
		return nil, domain.ErrValidation
	}
	current, ok := r.sets[monitorID]
	if !ok {
		// Mirror the real store: monitor creation initializes the local
		// assignment in the same transaction, so a set always exists at
		// revision one before a declarative commit touches it.
		if expectedRevision != 1 {
			return nil, ports.ErrNotFound
		}
		current = &domain.MonitorProbeAssignments{
			MonitorID: monitorID, Revision: 1, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Generation: 1}},
		}
		r.sets[monitorID] = current
		r.generation[monitorID] = map[string]int64{domain.LocalProbeID: 1}
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
		if id == domain.LocalProbeID {
			// The reserved local row exists on every install and is enabled.
		} else if p, err := r.registry.GetByID(context.Background(), id); err != nil {
			return nil, domain.ErrValidation
		} else if requireEnabled && !p.Enabled {
			return nil, domain.ErrValidation
		}
		gen := r.generation[monitorID][id]
		active := false
		for _, a := range current.Assignments {
			if a.ProbeID == id {
				active = true
			}
		}
		switch {
		case active:
			// retained member keeps its generation
		case gen > 0:
			gen++
		default:
			gen = 1
		}
		if r.generation[monitorID] == nil {
			r.generation[monitorID] = map[string]int64{}
		}
		r.generation[monitorID][id] = gen
		a := domain.ProbeAssignment{MonitorID: monitorID, ProbeID: id, Generation: gen}
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

func (r *backupFakeAssignmentRepo) ExecutableByLocal(_ context.Context, monitorIDs []int64) (map[int64]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]int64{}
	for _, id := range monitorIDs {
		set, ok := r.sets[id]
		if !ok {
			out[id] = 1
			continue
		}
		for _, a := range set.Assignments {
			if a.ProbeID == domain.LocalProbeID {
				out[id] = a.Generation
			}
		}
	}
	return out, nil
}

func (r *backupFakeAssignmentRepo) ListHistory(context.Context, int64, time.Time, time.Time) ([]domain.AssignmentInterval, error) {
	return nil, nil
}

// --- harness ---------------------------------------------------------------

func newBackupProbeHarness() (*backupHarness, *backupFakeProbeRegistry, *backupFakeAssignmentRepo) {
	h := newBackupHarness()
	registry := newBackupFakeProbeRegistry()
	assignments := newBackupFakeAssignmentRepo(registry)
	h.svc.SetProbeRegistry(registry)
	h.svc.SetProbeAssignments(assignments)
	return h, registry, assignments
}

const backupProbeTestID = "9a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"

func seedProbe(t *testing.T, registry *backupFakeProbeRegistry, id string) *domain.Probe {
	t.Helper()
	p := &domain.Probe{ID: id, Key: "us-east", Name: "US East", Location: "dc-1", Kind: domain.ProbeKindRemote, Enabled: true}
	if err := registry.Create(context.Background(), p); err != nil {
		t.Fatalf("seed probe us-east: %v", err)
	}
	return p
}

func seedMonitorWithSet(t *testing.T, h *backupHarness, assignments *backupFakeAssignmentRepo, userID int64, members []string, policy domain.HealthPolicy) {
	t.Helper()
	ctx := context.Background()
	m := &domain.Monitor{UserID: userID, Name: "regional", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{"url": "https://example.com"}}
	if err := h.monitors.Create(ctx, m); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}
	if _, err := assignments.InitializeLocal(ctx, m.ID); err != nil {
		t.Fatalf("init set: %v", err)
	}
	set, err := assignments.GetByMonitorID(ctx, m.ID)
	if err != nil {
		t.Fatalf("read set: %v", err)
	}
	if _, err := assignments.Replace(ctx, m.ID, set.Revision, members, policy); err != nil {
		t.Fatalf("seed set: %v", err)
	}
}

// --- tests -----------------------------------------------------------------

func TestBackupService_ExportImport_ProbeAssignmentsRoundTrip(t *testing.T) {
	ctx := context.Background()
	src, srcRegistry, srcAssignments := newBackupProbeHarness()
	const userID int64 = 1
	probe := seedProbe(t, srcRegistry, backupProbeTestID)
	seedMonitorWithSet(t, src, srcAssignments, userID, []string{domain.LocalProbeID, probe.ID}, domain.HealthPolicyAllDown)

	doc, err := src.svc.Export(ctx, userID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(doc.Probes) != 1 || doc.Probes[0].Key != "us-east" || doc.Probes[0].ID != backupProbeTestID {
		t.Fatalf("exported probes: %+v", doc.Probes)
	}
	if len(doc.MonitorProbeAssignments) != 1 || doc.MonitorProbeAssignments[0].HealthPolicy != domain.HealthPolicyAllDown {
		t.Fatalf("exported sets: %+v", doc.MonitorProbeAssignments)
	}
	if len(doc.MonitorProbeAssignments[0].Members) != 2 {
		t.Fatalf("exported members: %+v", doc.MonitorProbeAssignments[0].Members)
	}

	dst, dstRegistry, dstAssignments := newBackupProbeHarness()
	summary, err := dst.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.ProbesCreated != 1 || summary.ProbesReused != 0 || summary.MonitorProbeSetsRestored != 1 || summary.MonitorsCreated != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	restored, err := dstRegistry.GetByKey(ctx, "us-east")
	if err != nil {
		t.Fatalf("restored probe: %v", err)
	}
	if restored.ID != backupProbeTestID {
		t.Fatalf("restored identity changed: %+v", restored)
	}
	if restored.Enabled {
		t.Fatal("restored identity must stay disabled pending reenrollment")
	}
	if len(summary.Probes) != 1 || summary.Probes[0].ID != backupProbeTestID || summary.Probes[0].Reused {
		t.Fatalf("probe outcomes: %+v", summary.Probes)
	}

	// The imported monitor carries the declared set — not the placeholder
	// local assignment it was created with.
	var monitorID int64
	for id, mon := range dst.monitors.byID {
		if mon.Name == "regional" {
			monitorID = id
		}
	}
	set, err := dstAssignments.GetByMonitorID(ctx, monitorID)
	if err != nil {
		t.Fatalf("restored set: %v", err)
	}
	if set.HealthPolicy != domain.HealthPolicyAllDown || len(set.Assignments) != 2 {
		t.Fatalf("restored set: %+v", set)
	}
	seen := map[string]bool{}
	for _, a := range set.Assignments {
		seen[a.ProbeID] = true
	}
	if !seen[domain.LocalProbeID] || !seen[backupProbeTestID] {
		t.Fatalf("restored members: %+v", set.Assignments)
	}

	// Re-import: the identity is reused, never duplicated.
	summary2, err := dst.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("second import: %v", err)
	}
	if summary2.ProbesCreated != 0 || summary2.ProbesReused != 1 {
		t.Fatalf("second import summary: %+v", summary2)
	}
	probes, err := dstRegistry.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(probes) != 1 {
		t.Fatalf("duplicate live probe identity after re-import: %+v", probes)
	}
}

func TestBackupService_Import_RemoteOnlySetNeverReroutesToHub(t *testing.T) {
	ctx := context.Background()
	src, srcRegistry, srcAssignments := newBackupProbeHarness()
	const userID int64 = 1
	probe := seedProbe(t, srcRegistry, backupProbeTestID)
	seedMonitorWithSet(t, src, srcAssignments, userID, []string{probe.ID}, domain.HealthPolicyAnyDown)

	doc, err := src.svc.Export(ctx, userID)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	dst, _, dstAssignments := newBackupProbeHarness()
	summary, err := dst.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.MonitorProbeSetsRestored != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	var monitorID int64
	for id := range dst.monitors.byID {
		monitorID = id
	}
	set, err := dstAssignments.GetByMonitorID(ctx, monitorID)
	if err != nil || len(set.Assignments) != 1 || set.Assignments[0].ProbeID != backupProbeTestID {
		t.Fatalf("restored set: %+v %v", set, err)
	}
	allowed, err := dstAssignments.ExecutableByLocal(ctx, []int64{monitorID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := allowed[monitorID]; ok {
		t.Fatal("restored remote-only monitor became locally executable")
	}
}

func TestBackupService_Import_SkipsMonitorWhenProbeMissing(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1
	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		Monitors: []BackupMonitor{
			{ID: 7, Name: "regional", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
		MonitorProbeAssignments: []BackupMonitorAssignmentSet{
			{MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown, Members: []BackupMonitorAssignmentMember{{ProbeKey: "eu-west"}}},
		},
	}
	dst, _, _ := newBackupProbeHarness()
	summary, err := dst.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.MonitorsCreated != 0 || len(dst.monitors.byID) != 0 {
		t.Fatalf("monitor imported despite unresolvable probe: %+v", summary)
	}
	if len(summary.Skipped) != 1 || !strings.Contains(summary.Skipped[0].Reason, "refusing to import it as local") {
		t.Fatalf("skips: %+v", summary.Skipped)
	}
}

func TestBackupService_Import_SkipsAssignedMonitorWhenProbesUnavailable(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1
	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		Probes: []BackupProbe{
			{ID: backupProbeTestID, Key: "us-east", Name: "US East", Kind: domain.ProbeKindRemote},
		},
		Monitors: []BackupMonitor{
			{ID: 7, Name: "regional", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
		MonitorProbeAssignments: []BackupMonitorAssignmentSet{
			{MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown, Members: []BackupMonitorAssignmentMember{{ProbeKey: "us-east"}}},
		},
	}
	h := newBackupHarness() // probe stores deliberately absent
	summary, err := h.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.MonitorsCreated != 0 || summary.ProbesCreated != 0 || len(h.monitors.byID) != 0 {
		t.Fatalf("import without probe stores must refuse assigned monitors: %+v", summary)
	}
	if len(summary.Skipped) != 1 || !strings.Contains(summary.Skipped[0].Reason, "refusing to import it as local") {
		t.Fatalf("skips: %+v", summary.Skipped)
	}
}

func TestBackupService_Import_RemovesMonitorWhenSetRestoreFails(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1
	doc := &BackupDocument{
		Version: BackupDocumentVersion,
		Probes: []BackupProbe{
			{ID: backupProbeTestID, Key: "us-east", Name: "US East", Kind: domain.ProbeKindRemote},
		},
		Monitors: []BackupMonitor{
			{ID: 7, Name: "regional", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
		MonitorProbeAssignments: []BackupMonitorAssignmentSet{
			{MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown, Members: []BackupMonitorAssignmentMember{{ProbeKey: "us-east"}}},
		},
	}
	dst, _, dstAssignments := newBackupProbeHarness()
	dstAssignments.restoreErr = errors.New("storage unavailable")
	summary, err := dst.svc.Import(ctx, userID, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if summary.MonitorsCreated != 0 || len(dst.monitors.byID) != 0 {
		t.Fatalf("monitor survived an unrestorable assignment set: %+v", summary)
	}
	if len(summary.Skipped) != 1 || !strings.Contains(summary.Skipped[0].Reason, "monitor was not imported") {
		t.Fatalf("skips: %+v", summary.Skipped)
	}
}

func TestBackupService_Import_ReusesIdentityByKeyAndRefusesConflicts(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1

	t.Run("reuses existing key", func(t *testing.T) {
		dst, dstRegistry, _ := newBackupProbeHarness()
		existing := seedProbe(t, dstRegistry, "11111111-1111-4111-8111-111111111111")
		doc := &BackupDocument{
			Version: BackupDocumentVersion,
			Probes: []BackupProbe{
				{ID: backupProbeTestID, Key: "us-east", Name: "US East", Kind: domain.ProbeKindRemote},
			},
		}
		summary, err := dst.svc.Import(ctx, userID, doc)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if summary.ProbesCreated != 0 || summary.ProbesReused != 1 || summary.Probes[0].ID != existing.ID {
			t.Fatalf("summary: %+v", summary)
		}
	})

	t.Run("refuses identity carrying another key", func(t *testing.T) {
		dst, dstRegistry, _ := newBackupProbeHarness()
		seedProbe(t, dstRegistry, backupProbeTestID)
		doc := &BackupDocument{
			Version: BackupDocumentVersion,
			Probes: []BackupProbe{
				{ID: backupProbeTestID, Key: "eu-west", Name: "EU West", Kind: domain.ProbeKindRemote},
			},
		}
		summary, err := dst.svc.Import(ctx, userID, doc)
		if err != nil {
			t.Fatalf("import: %v", err)
		}
		if summary.ProbesCreated != 0 || summary.ProbesReused != 0 {
			t.Fatalf("conflicting identity must not be merged: %+v", summary)
		}
		if len(summary.Skipped) != 1 || summary.Skipped[0].Kind != "probe" {
			t.Fatalf("skips: %+v", summary.Skipped)
		}
	})
}

func TestBackupService_Import_AcceptsV1AndRejectsUnknownVersions(t *testing.T) {
	ctx := context.Background()
	const userID int64 = 1
	dst, _, dstAssignments := newBackupProbeHarness()
	v1 := &BackupDocument{
		Version: 1,
		Monitors: []BackupMonitor{
			{ID: 3, Name: "legacy", Type: "http", Active: true, Interval: 60, Timeout: 30, Config: map[string]any{}},
		},
	}
	summary, err := dst.svc.Import(ctx, userID, v1)
	if err != nil {
		t.Fatalf("v1 import: %v", err)
	}
	if summary.MonitorsCreated != 1 {
		t.Fatalf("v1 summary: %+v", summary)
	}
	for id := range dst.monitors.byID {
		set, err := dstAssignments.GetByMonitorID(ctx, id)
		if isNotFound(err) {
			continue // legacy monitor without a stored set is locally runnable
		}
		if err != nil || !domain.LocalWorkerMayRun(set) {
			t.Fatalf("v1 monitor must stay on the legacy local assignment: %+v %v", set, err)
		}
	}
	if _, err := dst.svc.Import(ctx, userID, &BackupDocument{Version: 3}); err == nil {
		t.Fatal("expected error for unsupported version")
	}
}

func TestBackupProbeWireShape_ExcludesRuntimeSecrets(t *testing.T) {
	raw, err := json.Marshal(BackupProbe{ID: backupProbeTestID, Key: "us-east", Name: "US East", Location: "dc-1", Kind: domain.ProbeKindRemote})
	if err != nil {
		t.Fatal(err)
	}
	var probeFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probeFields); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "key", "name", "location", "kind"}
	if len(probeFields) != len(want) {
		t.Fatalf("probe wire shape changed: %s", raw)
	}
	for _, f := range want {
		if _, ok := probeFields[f]; !ok {
			t.Fatalf("probe wire shape missing %s: %s", f, raw)
		}
	}

	raw, err = json.Marshal(BackupMonitorAssignmentSet{
		MonitorID: 1, HealthPolicy: domain.HealthPolicyAnyDown,
		Members: []BackupMonitorAssignmentMember{{ProbeKey: "us-east", BindingKey: "docker-main", BindingKind: "docker_socket"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var setFields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &setFields); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"monitor_id", "health_policy", "members"} {
		if _, ok := setFields[f]; !ok {
			t.Fatalf("assignment set wire shape missing %s: %s", f, raw)
		}
	}
	for _, forbidden := range []string{"credential", "token", "secret", "password", "queue", "endpoint", "fingerprint", "session"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("assignment wire shape leaks %q: %s", forbidden, raw)
		}
	}
}
