package services

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const assignmentTestProbe = "11111111-1111-4111-8111-111111111111"

const assignmentDisabledProbe = "33333333-3333-4333-8333-333333333333"

type fakeAssignmentWriter struct {
	ports.ProbeAssignmentWriter
	replaced  bool
	created   bool
	ids       []string
	bindings  []domain.ProbeAssignmentBinding
	policy    domain.HealthPolicy
	delivery  domain.AlertDelivery
	result    *domain.MonitorProbeAssignments
	err       error
	createErr error
}

func (f *fakeAssignmentWriter) ReplaceWithBindings(_ context.Context, _ int64, _ int64, probeIDs []string, policy domain.HealthPolicy, delivery domain.AlertDelivery, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.replaced, f.ids, f.policy, f.delivery, f.bindings = true, probeIDs, policy, delivery, bindings
	return f.result, nil
}

func (f *fakeAssignmentWriter) CreateMonitorWithAssignments(_ context.Context, m *domain.Monitor, probeIDs []string, policy domain.HealthPolicy, delivery domain.AlertDelivery, bindings []domain.ProbeAssignmentBinding) (*domain.MonitorProbeAssignments, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	f.created, f.ids, f.policy, f.delivery, f.bindings = true, probeIDs, policy, delivery, bindings
	m.ID = 42
	return f.result, nil
}

type fakeProbeLookup struct {
	ports.ProbeRegistryRepository
	probes map[string]domain.Probe
}

func (f fakeProbeLookup) GetByID(_ context.Context, id string) (*domain.Probe, error) {
	probe, ok := f.probes[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	return &probe, nil
}

type fakeMonitorLookup struct {
	ports.MonitorRepository
	monitor *domain.Monitor
}

func (f fakeMonitorLookup) GetByID(_ context.Context, id int64) (*domain.Monitor, error) {
	if f.monitor == nil || f.monitor.ID != id {
		return nil, ports.ErrNotFound
	}
	return f.monitor, nil
}

type fakeAssignmentCapabilities struct{ remote, proxy map[string]bool }

func (f fakeAssignmentCapabilities) RemoteCapable(kind string) bool { return f.remote[kind] }
func (f fakeAssignmentCapabilities) ProxyCapable(kind string) bool  { return f.proxy[kind] }

type fakeAssignmentRepo struct {
	ports.MonitorProbeAssignmentRepository
	set *domain.MonitorProbeAssignments
}

func (f fakeAssignmentRepo) GetByMonitorID(_ context.Context, _ int64) (*domain.MonitorProbeAssignments, error) {
	if f.set == nil {
		return nil, ports.ErrNotFound
	}
	return f.set, nil
}

// awareFleetReadiness is the permissive ports.HubWorkerReadiness double for tests
// that are not about the T34 gate: every live worker attests assignment
// ownership, so remote activation is allowed. The dedicated gate tests substitute
// their own double.
type awareFleetReadiness struct{}

func (awareFleetReadiness) DeclareWorker(context.Context, string, int, time.Duration) error {
	return nil
}

func (awareFleetReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	return nil, nil
}

// awareFleetGate builds a gate that permits remote activation.
func awareFleetGate() FleetActivationGate {
	return NewFleetActivationGate(awareFleetReadiness{}, time.Minute)
}

func assignmentTestCaps() ports.ProbeAssignmentCapabilities {
	return fakeAssignmentCapabilities{
		remote: map[string]bool{"http": true, "tcp": true, "docker": true, "ping": true},
		proxy:  map[string]bool{"http": true, "s3": true},
	}
}

func assignmentTestProbes() fakeProbeLookup {
	return fakeProbeLookup{probes: map[string]domain.Probe{
		domain.LocalProbeID:     {ID: domain.LocalProbeID, Kind: domain.ProbeKindLocal, Enabled: true},
		assignmentTestProbe:     {ID: assignmentTestProbe, Kind: domain.ProbeKindRemote, Enabled: true},
		assignmentDisabledProbe: {ID: assignmentDisabledProbe, Kind: domain.ProbeKindRemote, Enabled: false},
	}}
}

// TestValidateDesiredAssignments pins the complete-set write policy: every
// rejection happens before any commit and every rule is evidence-based.
func TestValidateDesiredAssignments(t *testing.T) {
	probes := assignmentTestProbes()
	caps := assignmentTestCaps()
	binding := domain.ProbeAssignmentBinding{ProbeID: assignmentTestProbe, ProbeResourceBinding: domain.ProbeResourceBinding{Kind: "docker_socket", BindingKey: "docker-main"}}
	bound := []domain.ProbeAssignmentBinding{binding}
	httpMonitor := &domain.Monitor{ID: 7, Type: "http"}
	dockerMonitor := &domain.Monitor{ID: 7, Type: "docker"}
	pushMonitor := &domain.Monitor{ID: 7, Type: "push"}
	proxyHTTP := &domain.Monitor{ID: 7, Type: "http", ProxyID: new(int64)}
	proxyTCP := &domain.Monitor{ID: 7, Type: "tcp", ProxyID: new(int64)}

	for _, tc := range []struct {
		name     string
		monitor  *domain.Monitor
		ids      []string
		policy   domain.HealthPolicy
		bindings *[]domain.ProbeAssignmentBinding
		delivery string
		want     error
	}{
		{"empty set", httpMonitor, nil, domain.HealthPolicyAnyDown, nil, "", ErrInvalidProbeIDs},
		{"duplicate members", httpMonitor, []string{domain.LocalProbeID, domain.LocalProbeID}, domain.HealthPolicyAnyDown, nil, "", ErrInvalidProbeIDs},
		{"malformed member", httpMonitor, []string{"not-a-probe"}, domain.HealthPolicyAnyDown, nil, "", ErrInvalidProbeIDs},
		{"bad policy", httpMonitor, []string{domain.LocalProbeID}, "quorum", nil, "", ErrInvalidPolicy},
		{"bad delivery", httpMonitor, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, nil, "hub", ErrInvalidDelivery},
		{"aggregate desired", httpMonitor, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, nil, "aggregate", nil},
		{"both desired", httpMonitor, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, nil, "both", nil},
		{"unknown probe", httpMonitor, []string{"99999999-9999-4999-8999-999999999999"}, domain.HealthPolicyAnyDown, nil, "", ErrUnknownProbe},
		{"disabled probe", httpMonitor, []string{assignmentDisabledProbe}, domain.HealthPolicyAnyDown, nil, "", ErrProbeUnavailable},
		{"push remote", pushMonitor, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", ErrUnsupportedAssignment},
		{"unavailable capability", &domain.Monitor{ID: 7, Type: "grpc"}, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", ErrUnsupportedAssignment},
		{"docker remote without binding", dockerMonitor, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", ErrUnsupportedAssignment},
		{"binding on non-docker", httpMonitor, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, &bound, "", ErrUnsupportedAssignment},
		{"binding on local", dockerMonitor, []string{domain.LocalProbeID, assignmentTestProbe}, domain.HealthPolicyAnyDown, &[]domain.ProbeAssignmentBinding{{ProbeID: domain.LocalProbeID, ProbeResourceBinding: domain.ProbeResourceBinding{Kind: "docker_socket", BindingKey: "docker-main"}}}, "", ErrInvalidBindings},
		{"binding for non-member", dockerMonitor, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, &bound, "", ErrInvalidBindings},
		{"proxy on ignoring checker", proxyTCP, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", ErrUnsupportedAssignment},
		{"local only", httpMonitor, []string{domain.LocalProbeID}, domain.HealthPolicyAllDown, nil, "regional", nil},
		{"remote http", httpMonitor, []string{domain.LocalProbeID, assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", nil},
		{"docker remote bound", dockerMonitor, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, &bound, "", nil},
		{"proxy on capable checker", proxyHTTP, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDesiredAssignments(t.Context(), tc.monitor, nil, tc.ids, tc.policy, tc.bindings, tc.delivery, probes, caps)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}

	t.Run("OmittedBindingsPreserveRetainedDocker", func(t *testing.T) {
		previous := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: assignmentTestProbe, Generation: 2, ResourceBinding: &binding.ProbeResourceBinding}}}
		if err := ValidateDesiredAssignments(t.Context(), dockerMonitor, previous, []string{assignmentTestProbe}, domain.HealthPolicyAnyDown, nil, "", probes, caps); err != nil {
			t.Fatalf("retained binding must survive omission: %v", err)
		}
		// A NEW member still needs its explicit binding.
		probes.probes["22222222-2222-4222-8222-222222222222"] = domain.Probe{ID: "22222222-2222-4222-8222-222222222222", Kind: domain.ProbeKindRemote, Enabled: true}
		err := ValidateDesiredAssignments(t.Context(), dockerMonitor, previous, []string{assignmentTestProbe, "22222222-2222-4222-8222-222222222222"}, domain.HealthPolicyAnyDown, nil, "", probes, caps)
		if !errors.Is(err, ErrUnsupportedAssignment) {
			t.Fatalf("new docker member without binding: %v", err)
		}
	})
}

func TestProbeAssignmentServiceReplace(t *testing.T) {
	monitor := &domain.Monitor{ID: 7, Type: "http"}
	previous := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 4, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}}
	newSet := func(revision int64, policy domain.HealthPolicy, members ...string) *domain.MonitorProbeAssignments {
		set := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: revision, HealthPolicy: policy}
		for _, id := range members {
			set.Assignments = append(set.Assignments, domain.ProbeAssignment{MonitorID: 7, ProbeID: id, Generation: 1})
		}
		return set
	}

	t.Run("StaleRevisionWritesNothing", func(t *testing.T) {
		writer := &fakeAssignmentWriter{}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		_, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 3, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown})
		if !errors.Is(err, ErrStaleRevision) || writer.replaced {
			t.Fatalf("stale write: %v replaced=%v", err, writer.replaced)
		}
		// Revision zero is never a valid precondition.
		if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 0, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown}); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("revision zero accepted: %v", err)
		}
	})

	t.Run("ConflictBecomesStaleRevision", func(t *testing.T) {
		writer := &fakeAssignmentWriter{err: ports.ErrConflict}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown}); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("conflict mapping: %v", err)
		}
	})

	t.Run("LegacyMonitorInitializesAtRevisionOne", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: newSet(1, domain.HealthPolicyAnyDown, domain.LocalProbeID)}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 5, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown}); !errors.Is(err, ErrStaleRevision) {
			t.Fatalf("legacy precondition must expect revision one: %v", err)
		}
		result, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 1, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown})
		if err != nil || !writer.replaced {
			t.Fatalf("legacy replacement: %v", err)
		}
		if len(result.PendingProbes) != 0 {
			t.Fatalf("local-only write pending: %v", result.PendingProbes)
		}
	})

	t.Run("PendingMarksChangedMembersOnly", func(t *testing.T) {
		binding := domain.ProbeResourceBinding{Kind: "docker_socket", BindingKey: "docker-main"}
		previous := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 4, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{
				{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1},
				{MonitorID: 7, ProbeID: assignmentTestProbe, Generation: 2, ResourceBinding: &binding},
			}}
		docker := &domain.Monitor{ID: 7, Type: "docker"}
		kept := domain.ProbeAssignmentBinding{ProbeID: assignmentTestProbe, ProbeResourceBinding: binding}
		writer := &fakeAssignmentWriter{result: &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 5, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{
				{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1},
				{MonitorID: 7, ProbeID: assignmentTestProbe, Generation: 2, ResourceBinding: &binding},
				{MonitorID: 7, ProbeID: "22222222-2222-4222-8222-222222222222", Generation: 1},
			}}}
		probes := assignmentTestProbes()
		probes.probes["22222222-2222-4222-8222-222222222222"] = domain.Probe{ID: "22222222-2222-4222-8222-222222222222", Kind: domain.ProbeKindRemote, Enabled: true}
		newBinding := domain.ProbeAssignmentBinding{ProbeID: "22222222-2222-4222-8222-222222222222", ProbeResourceBinding: domain.ProbeResourceBinding{Kind: "docker_api", BindingKey: "docker-api"}}
		bindings := []domain.ProbeAssignmentBinding{kept, newBinding}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, probes, fakeMonitorLookup{monitor: docker}, assignmentTestCaps(), awareFleetGate())
		result, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4,
			ProbeIDs:     []string{domain.LocalProbeID, assignmentTestProbe, "22222222-2222-4222-8222-222222222222"},
			HealthPolicy: domain.HealthPolicyAnyDown, Bindings: &bindings})
		if err != nil {
			t.Fatal(err)
		}
		if !result.PendingProbes["22222222-2222-4222-8222-222222222222"] || result.PendingProbes[assignmentTestProbe] || result.PendingProbes[domain.LocalProbeID] {
			t.Fatalf("pending must cover only changed members: %v", result.PendingProbes)
		}
		// A policy change makes every member unproven.
		writer.result = &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 6, HealthPolicy: domain.HealthPolicyAllDown,
			Assignments: previous.Assignments}
		result, err = svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4,
			ProbeIDs: []string{domain.LocalProbeID, assignmentTestProbe}, HealthPolicy: domain.HealthPolicyAllDown})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.PendingProbes) != 2 {
			t.Fatalf("policy change must mark everyone pending: %v", result.PendingProbes)
		}
	})

	t.Run("NoOpProvesNothingNew", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: previous}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		result, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown})
		if err != nil || len(result.PendingProbes) != 0 {
			t.Fatalf("no-op write: %v %v", result, err)
		}
		if writer.delivery != domain.AlertDeliveryRegional {
			t.Fatalf("omitted delivery was not stored as regional: %s", writer.delivery)
		}
	})

	t.Run("OmittedDeliveryPreservesAggregate", func(t *testing.T) {
		stored := *previous
		stored.AlertDelivery = domain.AlertDeliveryAggregate
		writer := &fakeAssignmentWriter{result: &stored}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: &stored}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		result, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown})
		if err != nil || writer.delivery != domain.AlertDeliveryAggregate || len(result.PendingProbes) != 0 {
			t.Fatalf("omission changed desired mode: delivery=%s pending=%v err=%v", writer.delivery, result.PendingProbes, err)
		}
	})

	t.Run("ModeChangeMarksEveryMemberPending", func(t *testing.T) {
		next := *previous
		next.Revision = 5
		next.AlertDelivery = domain.AlertDeliveryBoth
		writer := &fakeAssignmentWriter{result: &next}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), awareFleetGate())
		result, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: "both"})
		if err != nil || writer.delivery != domain.AlertDeliveryBoth || len(result.PendingProbes) != 1 || !result.PendingProbes[domain.LocalProbeID] {
			t.Fatalf("mode change pending: delivery=%s pending=%v err=%v", writer.delivery, result.PendingProbes, err)
		}
	})
}

// TestMonitorServiceCloneAuthority proves a remote clone is rejected for a
// non-admin instead of silently rerouted, and that an admin clone reproduces
// the source's complete desired set atomically.
func TestProbeAssignmentSetEqualIncludesDelivery(t *testing.T) {
	current := &domain.MonitorProbeAssignments{
		HealthPolicy: domain.HealthPolicyAnyDown, AlertDelivery: domain.AlertDeliveryAggregate,
		Assignments: []domain.ProbeAssignment{{ProbeID: domain.LocalProbeID}},
	}
	if probeAssignmentSetEqual(current, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryRegional, nil) {
		t.Fatal("regional delivery matched a stored aggregate set")
	}
	if !probeAssignmentSetEqual(current, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryAggregate, nil) {
		t.Fatal("aggregate delivery did not match")
	}
	empty := *current
	empty.AlertDelivery = ""
	if !probeAssignmentSetEqual(&empty, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryRegional, nil) {
		t.Fatal("absent stored delivery was not treated as regional")
	}
}

func TestMonitorServiceCloneAuthority(t *testing.T) {
	repo := newCloneFakeMonitorRepo()
	bus := newFakeBus()
	svc := NewMonitorService(repo, bus)
	src := &domain.Monitor{UserID: 1, Name: "Regional", Type: "docker", Active: true, Interval: 60}
	if err := svc.Create(t.Context(), src); err != nil {
		t.Fatal(err)
	}
	binding := domain.ProbeResourceBinding{Kind: "docker_socket", BindingKey: "docker-main"}
	set := &domain.MonitorProbeAssignments{MonitorID: src.ID, Revision: 2, HealthPolicy: domain.HealthPolicyAllDown, AlertDelivery: domain.AlertDeliveryAggregate,
		Assignments: []domain.ProbeAssignment{
			{MonitorID: src.ID, ProbeID: domain.LocalProbeID, Generation: 1},
			{MonitorID: src.ID, ProbeID: assignmentTestProbe, Generation: 1, ResourceBinding: &binding},
		}}
	writer := &fakeAssignmentWriter{result: set}
	svc.SetAssignmentProvisioning(writer, assignmentTestProbes(), assignmentTestCaps(), awareFleetGate())
	svc.SetAssignmentReader(fakeAssignmentRepo{set: set})

	if _, err := svc.Clone(t.Context(), src.ID, 1, false); !errors.Is(err, ErrRemoteCloneForbidden) || writer.created {
		t.Fatalf("non-admin remote clone: %v created=%v", err, writer.created)
	}
	cloned, err := svc.Clone(t.Context(), src.ID, 1, true)
	if err != nil || !writer.created {
		t.Fatalf("admin remote clone: %v", err)
	}
	if cloned.ID == src.ID || strings.Join(writer.ids, ",") != domain.LocalProbeID+","+assignmentTestProbe || writer.policy != domain.HealthPolicyAllDown || writer.delivery != domain.AlertDeliveryAggregate || len(writer.bindings) != 1 || writer.bindings[0].BindingKey != "docker-main" {
		t.Fatalf("clone must reproduce the source set: id=%d ids=%v policy=%s delivery=%s bindings=%v", cloned.ID, writer.ids, writer.policy, writer.delivery, writer.bindings)
	}

	// A local-only clone still reproduces the source set through the
	// provisioner, preserving its policy instead of resetting it.
	writer.created = false
	local := &domain.MonitorProbeAssignments{MonitorID: src.ID, Revision: 3, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{{MonitorID: src.ID, ProbeID: domain.LocalProbeID, Generation: 1}}}
	svc.SetAssignmentReader(fakeAssignmentRepo{set: local})
	if _, err := svc.Clone(t.Context(), src.ID, 1, false); err != nil || !writer.created || strings.Join(writer.ids, ",") != domain.LocalProbeID {
		t.Fatalf("local clone for non-admin: %v created=%v ids=%v", err, writer.created, writer.ids)
	}
}
