package repository_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const restoreProbeID = "44444444-4444-4444-8444-444444444444"

var remoteUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validRemoteUUID(id string) bool {
	return remoteUUIDPattern.MatchString(id) && id != "00000000-0000-0000-0000-000000000000"
}

// TestProbeAssignmentRestore_AcceptsDisabledRegistration exercises the one
// documented difference between Replace and Restore: a declarative commit
// (backup import / config apply) may reference a registered-but-disabled probe
// — a restored identity is disabled pending reenrollment — while live operator
// replacement keeps refusing it. Everything else must be identical: complete
// sets, tombstoned removals, retained generations and local-execution gating.
func TestProbeAssignmentRestore_AcceptsDisabledRegistration(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitors := newEngineMonitorRepo(f)

			disabled := &domain.Probe{ID: restoreProbeID, Key: "us-east", Name: "US East", Location: "ashburn", Kind: domain.ProbeKindRemote, Enabled: false}
			if err := f.registry.Create(ctx, disabled); err != nil {
				t.Fatalf("create probe: %v", err)
			}
			m := &domain.Monitor{UserID: f.user(t), Name: "regional", Type: "http", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			if err := monitors.Create(ctx, m); err != nil {
				t.Fatalf("create monitor: %v", err)
			}

			// Live operator replacement must keep refusing a disabled member.
			if _, err := f.assignments.Replace(ctx, m.ID, 1, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAnyDown); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Replace accepted a disabled registration: %v", err)
			}

			// The declarative path accepts it and keeps `local` (this set has
			// both members).
			set, err := f.assignments.Restore(ctx, m.ID, 1, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAllDown, "", nil)
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if set.Revision != 2 || set.HealthPolicy != domain.HealthPolicyAllDown || len(set.Assignments) != 2 {
				t.Fatalf("restored set: %+v", set)
			}
			if !domain.LocalWorkerMayRun(set) {
				t.Fatal("local member lost hub execution")
			}

			// A remote-only set removes the hub from execution entirely.
			set, err = f.assignments.Restore(ctx, m.ID, 2, []string{restoreProbeID}, domain.HealthPolicyAllDown, "", nil)
			if err != nil {
				t.Fatalf("Restore remote-only: %v", err)
			}
			if set.Revision != 3 || len(set.Assignments) != 1 || set.Assignments[0].ProbeID != restoreProbeID {
				t.Fatalf("remote-only set: %+v", set)
			}
			allowed, err := f.assignments.ExecutableByLocal(ctx, []int64{m.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := allowed[m.ID]; ok {
				t.Fatal("remote-only monitor is still locally executable")
			}

			// Committing the identical set again is a documented no-op.
			again, err := f.assignments.Restore(ctx, m.ID, 3, []string{restoreProbeID}, domain.HealthPolicyAllDown, "", nil)
			if err != nil || again.Revision != 3 {
				t.Fatalf("idempotent restore: %+v %v", again, err)
			}

			// Re-adding the removed member retains generations: `local` was
			// tombstoned at generation one and must come back as two.
			set, err = f.assignments.Restore(ctx, m.ID, 3, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAllDown, "", nil)
			if err != nil {
				t.Fatalf("re-add: %v", err)
			}
			generation := map[string]int64{}
			for _, a := range set.Assignments {
				generation[a.ProbeID] = a.Generation
			}
			if generation[domain.LocalProbeID] != 2 || generation[restoreProbeID] != 1 {
				t.Fatalf("generations: %+v", generation)
			}

			// An unknown member is refused exactly like Replace: a declarative
			// commit never invents an identity. A missing registration is a
			// missing row, so it surfaces as ErrNotFound.
			if _, err := f.assignments.Restore(ctx, m.ID, set.Revision, []string{"55555555-5555-4555-8555-555555555555"}, domain.HealthPolicyAnyDown, "", nil); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("Restore accepted an unregistered member: %v", err)
			}
		})
	}
}

// TestProbeAssignmentRestore_KeepsDockerBindingContract proves Restore is not
// a bypass for the resource-binding rules: a remote Docker assignment still
// needs an explicit probe-local binding, and the local member still cannot
// carry one.
func TestProbeAssignmentRestore_KeepsDockerBindingContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitors := newEngineMonitorRepo(f)

			probe := &domain.Probe{ID: restoreProbeID, Key: "us-east", Name: "US East", Kind: domain.ProbeKindRemote, Enabled: false}
			if err := f.registry.Create(ctx, probe); err != nil {
				t.Fatalf("create probe: %v", err)
			}
			m := &domain.Monitor{UserID: f.user(t), Name: "docker", Type: "docker", Active: true, Interval: 60, Timeout: 5, Config: map[string]any{}}
			if err := monitors.Create(ctx, m); err != nil {
				t.Fatalf("create monitor: %v", err)
			}

			if _, err := f.assignments.Restore(ctx, m.ID, 1, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAnyDown, "", nil); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Restore dropped the docker binding requirement: %v", err)
			}
			binding := domain.ProbeAssignmentBinding{
				ProbeID:              restoreProbeID,
				ProbeResourceBinding: domain.ProbeResourceBinding{BindingKey: "docker-main", Kind: "docker_socket"},
			}
			set, err := f.assignments.Restore(ctx, m.ID, 1, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAnyDown, "", []domain.ProbeAssignmentBinding{binding})
			if err != nil {
				t.Fatalf("Restore with binding: %v", err)
			}
			found := false
			for _, a := range set.Assignments {
				if a.ProbeID == restoreProbeID && a.ResourceBinding != nil && a.ResourceBinding.BindingKey == "docker-main" {
					found = true
				}
			}
			if !found {
				t.Fatalf("binding not persisted: %+v", set.Assignments)
			}

			localBinding := domain.ProbeAssignmentBinding{
				ProbeID:              domain.LocalProbeID,
				ProbeResourceBinding: domain.ProbeResourceBinding{BindingKey: "docker-main", Kind: "docker_socket"},
			}
			if _, err := f.assignments.Restore(ctx, m.ID, set.Revision, []string{domain.LocalProbeID, restoreProbeID}, domain.HealthPolicyAnyDown, "", []domain.ProbeAssignmentBinding{localBinding}); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Restore accepted a binding on the local member: %v", err)
			}
		})
	}
}

// TestProbeRegistryCreate_AllocatesIdentity proves declarative callers can omit
// the runtime identity: the store allocates a canonical UUID and the stable key
// stays the lookup handle. This is what lets config-as-code declare probes
// without minting UUIDs in core code.
func TestProbeRegistryCreate_AllocatesIdentity(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			p := &domain.Probe{Key: "eu-west", Name: "EU West", Location: "dublin", Kind: domain.ProbeKindRemote, Enabled: true}
			if err := f.registry.Create(ctx, p); err != nil {
				t.Fatalf("create: %v", err)
			}
			if !validRemoteUUID(p.ID) {
				t.Fatalf("store did not allocate a canonical identity: %q", p.ID)
			}
			byKey, err := f.registry.GetByKey(ctx, "eu-west")
			if err != nil || byKey.ID != p.ID {
				t.Fatalf("GetByKey: %+v %v", byKey, err)
			}
			byID, err := f.registry.GetByID(ctx, p.ID)
			if err != nil || byID.Key != "eu-west" || byID.Revision != 1 {
				t.Fatalf("GetByID: %+v %v", byID, err)
			}
		})
	}
}
