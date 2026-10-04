package repository_test

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestProbeResourceBindingContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			_, protector, monitor, _ := remoteSyncFixture(t, f)
			ctx := t.Context()
			store := repository.NewRemoteProbeConfigSyncStore(f.db, probe.RemoteConfigEncoder{}, probe.NewHubConfigDecoder(checker.Get, notifier.Get), protector)
			assignments := repository.NewProbeAssignmentStore(f.db)
			ids := []string{probeRegistryID1}
			policy := domain.HealthPolicyAnyDown
			binding := domain.ProbeAssignmentBinding{ProbeID: probeRegistryID1, ProbeResourceBinding: domain.ProbeResourceBinding{BindingKey: "docker", Kind: "docker_socket"}}
			if _, err := assignments.ReplaceWithBindings(ctx, monitor, 2, ids, policy, []domain.ProbeAssignmentBinding{binding}); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("non-Docker binding accepted", err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE monitors SET type = 'docker', config = ? WHERE id = ?", `{"container":"phoenix","docker_daemon":"unix:///private/hub.sock"}`, monitor); err != nil {
				t.Fatal(err)
			}
			if _, err := assignments.Replace(ctx, monitor, 2, ids, policy); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("missing Docker binding accepted", err)
			}
			bound, err := assignments.ReplaceWithBindings(ctx, monitor, 2, ids, policy, []domain.ProbeAssignmentBinding{binding})
			if err != nil || bound.Revision != 3 || *bound.Assignments[0].ResourceBinding != binding.ProbeResourceBinding {
				t.Fatalf("binding not saved: %+v %v", bound, err)
			}
			if _, err := assignments.ReplaceWithBindings(ctx, monitor, 3, ids, policy, []domain.ProbeAssignmentBinding{}); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("required binding cleared", err)
			}
			again, err := assignments.Replace(ctx, monitor, 3, ids, policy)
			if err != nil || !reflect.DeepEqual(bound, again) {
				t.Fatal("omitted bindings did not preserve/no-op", err)
			}
			if _, err := assignments.ReplaceWithBindings(ctx, monitor, 2, ids, policy, []domain.ProbeAssignmentBinding{binding}); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("stale revision changed binding", err)
			}
			meta, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
			if err != nil {
				t.Fatal(err)
			}
			retained, err := repository.NewProbeConfigStore(f.db).Latest(ctx, probeRegistryID1)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := protector.Open(ctx, meta, retained.ProtectedPayload)
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := probe.DecodeConfigSnapshot(plain)
			if err != nil || bytes.Contains(plain, []byte("hub.sock")) || len(snapshot.Assignments[0].ResourceBindings) != 1 || snapshot.Assignments[0].ResourceBindings[0].BindingKey != binding.BindingKey {
				t.Fatal("publication lost binding or exported endpoint", err)
			}
			// Publication races a reference replacement through real transaction
			// locks, then a fresh publication must reflect the committed reference.
			binding.BindingKey = "replacement"
			var wg sync.WaitGroup
			errs := make(chan error, 2)
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
				errs <- err
			}()
			go func() {
				defer wg.Done()
				_, err := assignments.ReplaceWithBindings(ctx, monitor, 3, ids, policy, []domain.ProbeAssignmentBinding{binding})
				errs <- err
			}()
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal("concurrent binding publication", err)
				}
			}
			updated, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC())
			if err != nil || updated.Revision != meta.Revision+1 {
				t.Fatal("binding edit did not publish", err)
			}
			current, err := assignments.GetByMonitorID(ctx, monitor)
			if err != nil || current.Revision != 4 || current.Assignments[0].Generation != bound.Assignments[0].Generation || current.Assignments[0].ResourceBinding.BindingKey != binding.BindingKey {
				t.Fatal("binding edit corrupted identity", err)
			}
			if _, err := assignments.Replace(ctx, monitor, 4, []string{domain.LocalProbeID}, policy); err != nil {
				t.Fatal(err)
			}
			if _, err := assignments.Replace(ctx, monitor, 5, ids, policy); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("tombstoned binding resurrected", err)
			}
			if set, err := assignments.ReplaceWithBindings(ctx, monitor, 5, ids, policy, []domain.ProbeAssignmentBinding{binding}); err != nil || set.Assignments[0].Generation != 2 {
				t.Fatal("reassignment lost generation", err)
			}
			// The populated additive migration preserves identity on down/up and
			// removes only its own references. Missing references fail closed.
			for _, direction := range []string{"down", "up"} {
				if err := runEngineMigration(t, f.db, engine, "065_probe_resource_bindings", direction); err != nil {
					t.Fatal(err)
				}
			}
			set, err := assignments.GetByMonitorID(ctx, monitor)
			if err != nil || set.Revision != 6 || set.Assignments[0].Generation != 2 || set.Assignments[0].ResourceBinding != nil {
				t.Fatal("migration changed assignment identity", err)
			}
			if _, err := store.RefreshRemote(ctx, syncTarget(), time.Now().UTC()); err == nil {
				t.Fatal("unbound Docker snapshot published after downgrade")
			}
		})
	}
}
