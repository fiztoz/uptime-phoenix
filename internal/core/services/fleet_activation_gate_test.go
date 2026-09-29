package services

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// fakeFleetReadiness is a controllable ports.HubWorkerReadiness double. It counts
// readiness queries so a test can prove the gate was not consulted at all for a
// local-only set, which is the property that keeps a default single-pod install
// from depending on fleet readiness.
type fakeFleetReadiness struct {
	unaware []string
	err     error
	calls   int
}

func (f *fakeFleetReadiness) DeclareWorker(context.Context, string, int, time.Duration) error {
	return nil
}

func (f *fakeFleetReadiness) UnawareWorkers(context.Context, int, time.Duration) ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.unaware, nil
}

func fleetGate(f *fakeFleetReadiness) FleetActivationGate {
	return NewFleetActivationGate(f, time.Minute)
}

// TestFleetActivationGate covers verification matrix T34: remote activation must
// be refused while any live hub worker cannot be shown to enforce assignment
// ownership, and must never be refused for a local-only set.
func TestFleetActivationGate(t *testing.T) {
	remote := []string{assignmentTestProbe}
	localOnly := []string{domain.LocalProbeID}
	mixed := []string{domain.LocalProbeID, assignmentTestProbe}

	t.Run("LocalOnlyNeverConsultsReadiness", func(t *testing.T) {
		f := &fakeFleetReadiness{unaware: []string{"worker-old"}}
		if err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), localOnly); err != nil {
			t.Fatalf("local-only set refused: %v", err)
		}
		if f.calls != 0 {
			t.Fatalf("local-only set consulted readiness %d times; it must not depend on fleet state", f.calls)
		}
		// An empty and a nil member list are local-only too: neither hands work
		// to a probe, so neither may be blocked by a rollout.
		for _, set := range [][]string{nil, {}} {
			if err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), set); err != nil {
				t.Fatalf("empty set refused: %v", err)
			}
		}
		if f.calls != 0 {
			t.Fatalf("empty set consulted readiness %d times", f.calls)
		}
	})

	t.Run("RemoteAllowedWhenEveryWorkerAttests", func(t *testing.T) {
		f := &fakeFleetReadiness{}
		if err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), remote); err != nil {
			t.Fatalf("aware fleet refused: %v", err)
		}
		if f.calls != 1 {
			t.Fatalf("readiness calls = %d, want 1", f.calls)
		}
		// A set that keeps local AND adds a probe is still a remote activation:
		// some probe outside this hub will execute the monitor.
		if err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), mixed); err != nil {
			t.Fatalf("mixed set refused on an aware fleet: %v", err)
		}
	})

	t.Run("RemoteRefusedAndNamesTheOffender", func(t *testing.T) {
		f := &fakeFleetReadiness{unaware: []string{"worker-old-7d9f"}}
		err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), remote)
		if !errors.Is(err, ErrFleetNotAssignmentAware) {
			t.Fatalf("unaware fleet: %v", err)
		}
		if !strings.Contains(err.Error(), "worker-old-7d9f") {
			t.Fatalf("refusal must name the offending worker for the operator: %v", err)
		}
	})

	t.Run("ReadinessFailureFailsClosed", func(t *testing.T) {
		f := &fakeFleetReadiness{err: errors.New("readiness table unreadable")}
		err := fleetGate(f).EnsureRemoteActivationAllowed(t.Context(), remote)
		if !errors.Is(err, ErrFleetReadinessUnavailable) {
			t.Fatalf("unreadable readiness: %v", err)
		}
		// The underlying cause stays in the chain for logs.
		if !strings.Contains(err.Error(), "readiness table unreadable") {
			t.Fatalf("cause not preserved: %v", err)
		}
	})

	// The zero value is what a caller that forgot to wire readiness gets. It must
	// refuse, not permit: a gate that can be forgotten has to fail loudly
	// (AGENTS.md rule 7 — never leave a path that reports success without doing
	// the work).
	t.Run("UnwiredGateFailsClosedOnRemote", func(t *testing.T) {
		var zero FleetActivationGate
		if err := zero.EnsureRemoteActivationAllowed(t.Context(), remote); !errors.Is(err, ErrFleetReadinessUnavailable) {
			t.Fatalf("unwired gate permitted remote activation: %v", err)
		}
		// A non-positive lookback is equally unusable, even with a store attached.
		if err := NewFleetActivationGate(&fakeFleetReadiness{}, 0).
			EnsureRemoteActivationAllowed(t.Context(), remote); !errors.Is(err, ErrFleetReadinessUnavailable) {
			t.Fatalf("zero lookback permitted remote activation: %v", err)
		}
		// Local-only stays allowed: the unwired gate must not break a
		// probes-disabled install.
		if err := zero.EnsureRemoteActivationAllowed(t.Context(), localOnly); err != nil {
			t.Fatalf("unwired gate refused a local-only set: %v", err)
		}
	})

	t.Run("RefusalBoundsTheNamedWorkerList", func(t *testing.T) {
		many := make([]string, 0, maxNamedUnawareWorkers+4)
		for i := range maxNamedUnawareWorkers + 4 {
			many = append(many, fmt.Sprintf("worker-%02d", i))
		}
		err := fleetGate(&fakeFleetReadiness{unaware: many}).EnsureRemoteActivationAllowed(t.Context(), remote)
		if !errors.Is(err, ErrFleetNotAssignmentAware) {
			t.Fatalf("bounded refusal: %v", err)
		}
		msg := err.Error()
		if !strings.Contains(msg, fmt.Sprintf("(+%d more)", len(many)-maxNamedUnawareWorkers)) {
			t.Fatalf("truncation not reported: %q", msg)
		}
		if strings.Contains(msg, fmt.Sprintf("worker-%02d", len(many)-1)) {
			t.Fatalf("message is not bounded: %q", msg)
		}
	})
}

// TestProbeAssignmentServiceReplaceFleetGate proves the gate sits in front of the
// write: a refused activation must leave the writer untouched, so no partial
// desired set can exist.
func TestProbeAssignmentServiceReplaceFleetGate(t *testing.T) {
	monitor := &domain.Monitor{ID: 7, Type: "http"}
	previous := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 4, HealthPolicy: domain.HealthPolicyAnyDown,
		Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: domain.LocalProbeID, Generation: 1}}}

	t.Run("UnawareFleetWritesNothing", func(t *testing.T) {
		writer := &fakeAssignmentWriter{}
		gate := fleetGate(&fakeFleetReadiness{unaware: []string{"worker-old"}})
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), gate)
		_, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{
			ExpectedRevision: 4, ProbeIDs: []string{assignmentTestProbe}, HealthPolicy: domain.HealthPolicyAnyDown,
		})
		if !errors.Is(err, ErrFleetNotAssignmentAware) {
			t.Fatalf("replace on an unaware fleet: %v", err)
		}
		// The load-bearing assertion is the effect, not the status: the writer was
		// never called, so the persisted desired set is unchanged.
		if writer.replaced || writer.ids != nil {
			t.Fatalf("refused activation reached the writer: replaced=%v ids=%v", writer.replaced, writer.ids)
		}
	})

	t.Run("AwareFleetCommits", func(t *testing.T) {
		committed := &domain.MonitorProbeAssignments{MonitorID: 7, Revision: 5, HealthPolicy: domain.HealthPolicyAnyDown,
			Assignments: []domain.ProbeAssignment{{MonitorID: 7, ProbeID: assignmentTestProbe, Generation: 1}}}
		writer := &fakeAssignmentWriter{result: committed}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), fleetGate(&fakeFleetReadiness{}))
		if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{
			ExpectedRevision: 4, ProbeIDs: []string{assignmentTestProbe}, HealthPolicy: domain.HealthPolicyAnyDown,
		}); err != nil || !writer.replaced {
			t.Fatalf("aware fleet must commit: %v replaced=%v", err, writer.replaced)
		}
	})

	t.Run("LocalOnlyReplaceStillCommitsWhenUnwired", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: previous}
		svc := NewProbeAssignmentService(writer, fakeAssignmentRepo{set: previous}, assignmentTestProbes(), fakeMonitorLookup{monitor: monitor}, assignmentTestCaps(), FleetActivationGate{})
		if _, err := svc.Replace(t.Context(), 7, ProbeAssignmentRequest{
			ExpectedRevision: 4, ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown,
		}); err != nil || !writer.replaced {
			t.Fatalf("local-only replace must not need readiness: %v replaced=%v", err, writer.replaced)
		}
	})
}

// TestMonitorServiceCreateWithAssignmentsFleetGate proves the second remote-write
// entry point is gated too, which is what also covers Clone (Clone reproduces a
// remote source set through CreateWithAssignments).
func TestMonitorServiceCreateWithAssignmentsFleetGate(t *testing.T) {
	newSvc := func(t *testing.T, writer *fakeAssignmentWriter, gate FleetActivationGate) *MonitorService {
		t.Helper()
		svc := NewMonitorService(newCloneFakeMonitorRepo(), newFakeBus())
		svc.SetAssignmentProvisioning(writer, assignmentTestProbes(), assignmentTestCaps(), gate)
		return svc
	}
	monitor := func() *domain.Monitor {
		return &domain.Monitor{UserID: 1, Name: "Regional", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{"url": "https://example.com"}}
	}
	initial := InitialAssignments{ProbeIDs: []string{assignmentTestProbe}, HealthPolicy: domain.HealthPolicyAnyDown}

	t.Run("UnawareFleetCreatesNothing", func(t *testing.T) {
		writer := &fakeAssignmentWriter{}
		svc := newSvc(t, writer, fleetGate(&fakeFleetReadiness{unaware: []string{"worker-old"}}))
		err := svc.CreateWithAssignments(t.Context(), monitor(), initial)
		if !errors.Is(err, ErrFleetNotAssignmentAware) {
			t.Fatalf("create on an unaware fleet: %v", err)
		}
		if writer.created {
			t.Fatal("refused activation reached the writer")
		}
	})

	t.Run("AwareFleetCreates", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: &domain.MonitorProbeAssignments{MonitorID: 1, Revision: 1}}
		svc := newSvc(t, writer, fleetGate(&fakeFleetReadiness{}))
		if err := svc.CreateWithAssignments(t.Context(), monitor(), initial); err != nil || !writer.created {
			t.Fatalf("aware fleet must create: %v created=%v", err, writer.created)
		}
	})

	t.Run("LocalOnlyCreateNeedsNoReadiness", func(t *testing.T) {
		writer := &fakeAssignmentWriter{result: &domain.MonitorProbeAssignments{MonitorID: 1, Revision: 1}}
		svc := newSvc(t, writer, FleetActivationGate{})
		local := InitialAssignments{ProbeIDs: []string{domain.LocalProbeID}, HealthPolicy: domain.HealthPolicyAnyDown}
		if err := svc.CreateWithAssignments(t.Context(), monitor(), local); err != nil || !writer.created {
			t.Fatalf("local-only create must not need readiness: %v created=%v", err, writer.created)
		}
	})
}
