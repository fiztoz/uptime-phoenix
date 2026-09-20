package repository_test

import (
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func runtimePeer(t *testing.T, f probeRegistryFixture) *repository.ProbeConnectorStore {
	t.Helper()
	var db *bun.DB
	var err error
	if f.engine == "sqlite" {
		db, err = sqlite.NewDB(f.dsn)
	} else {
		db, err = mariadb.NewDB(f.dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewProbeConnectorStore(db)
}

func TestProbeRuntimeOwnership(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "runtime")
			first, second := repository.NewProbeConnectorStore(f.db), runtimePeer(t, f)
			ctx := t.Context()
			start := make(chan struct{})
			var wg sync.WaitGroup
			leases := make([]domain.ProbeRuntimeLease, 2)
			errs := make([]error, 2)
			for i, store := range []*repository.ProbeConnectorStore{first, second} {
				wg.Add(1)
				go func(i int, store *repository.ProbeConnectorStore) {
					defer wg.Done()
					<-start
					leases[i], errs[i] = store.AcquireRuntime(ctx, probeRegistryID1, []string{probeRegistryID2, probeRegistryID3}[i])
				}(i, store)
			}
			close(start)
			wg.Wait()
			var live domain.ProbeRuntimeLease
			for i, err := range errs {
				if err == nil {
					if live.Epoch != 0 {
						t.Fatal("two workers own one runtime")
					}
					live = leases[i]
				} else if !errors.Is(err, ports.ErrConflict) {
					t.Fatal(err)
				}
			}
			if live.Epoch != 1 || live.LeaseUntil.Location() != time.UTC || live.LeaseUntil.Before(time.Now().UTC().Add(58*time.Second)) {
				t.Fatalf("invalid live owner: %+v", live)
			}
			if _, err := second.AcquireRuntime(ctx, live.ProbeID, live.OwnerID); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("same-owner duplicate reset epoch: %v", err)
			}
			child, err := first.AcquireRuntimeConnector(ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			if child.Generation != 1 || child.LeaseUntil.After(live.LeaseUntil) {
				t.Fatalf("child outlives owner: %+v", child)
			}
			if err := first.ReleaseConnector(ctx, child); err != nil {
				t.Fatal(err)
			}
			if _, err := second.AcquireRuntime(ctx, live.ProbeID, probeRegistryID1); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("backoff surrendered owner: %v", err)
			}
			next, err := second.AcquireRuntimeConnector(ctx, live)
			if err != nil || next.Generation != 2 {
				t.Fatalf("reconnect generation: %+v %v", next, err)
			}
			if _, err := first.RenewConnector(ctx, child); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("old connection revived: %v", err)
			}
			renewed, err := second.RenewRuntime(ctx, live)
			if err != nil || renewed.Epoch != live.Epoch {
				t.Fatalf("renew changed owner: %+v %v", renewed, err)
			}
			renewedChild, err := first.RenewConnector(ctx, next)
			if err != nil || renewedChild.LeaseUntil.After(renewed.LeaseUntil) {
				t.Fatalf("child renewal exceeds owner: %+v %v", renewedChild, err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until = 0 WHERE probe_id = ?", live.ProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := first.RenewRuntime(ctx, live); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expired owner revived: %v", err)
			}
			if _, err := first.RenewConnector(ctx, next); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("child bypassed expired parent: %v", err)
			}
			replacement, err := second.AcquireRuntime(ctx, live.ProbeID, live.OwnerID)
			if err != nil || replacement.Epoch != 2 {
				t.Fatalf("same-worker replacement failed: %+v %v", replacement, err)
			}
			current, err := second.AcquireRuntimeConnector(ctx, replacement)
			if err != nil || current.Generation != 3 {
				t.Fatalf("takeover discarded generation: %+v %v", current, err)
			}
			if err := second.SetConnectorConnected(ctx, current, true); err != nil {
				t.Fatal(err)
			}
			for name, mutate := range map[string]func() error{
				"renew":              func() error { _, err := first.RenewRuntime(ctx, live); return err },
				"release":            func() error { return first.ReleaseRuntime(ctx, live) },
				"connect":            func() error { _, err := first.AcquireRuntimeConnector(ctx, live); return err },
				"old child callback": func() error { return first.SetConnectorConnected(ctx, next, false) },
			} {
				if err := mutate(); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("stale %s accepted: %v", name, err)
				}
			}
			var connected bool
			if err := f.db.NewRaw("SELECT connected FROM probe_sessions WHERE probe_id = ?", live.ProbeID).Scan(ctx, &connected); err != nil || !connected {
				t.Fatalf("stale owner canceled replacement: %v %v", connected, err)
			}
			if err := second.ReleaseRuntime(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			if err := first.SetConnectorConnected(ctx, current, true); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("released owner retained live session: %v", err)
			}
			if _, err := first.AcquireConnector(ctx, live.ProbeID, live.OwnerID); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("legacy path bypassed released epoch: %v", err)
			}
			if err := runEngineMigration(t, f.db, engine, "058_probe_runtime_owners", "down"); err == nil {
				t.Fatal("downgrade discarded durable epoch")
			}
		})
	}
}

func TestProbeRuntimeAdoptsLegacyAndFencesDisable(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "adoption")
			store := repository.NewProbeConnectorStore(f.db)
			ctx := t.Context()
			legacy, err := store.AcquireConnector(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("stole live legacy connection: %v", err)
			}
			if err := store.ReleaseConnector(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			live, err := store.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireConnector(ctx, live.ProbeID, live.OwnerID); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("legacy bypassed active runtime: %v", err)
			}
			child, err := store.AcquireRuntimeConnector(ctx, live)
			if err != nil || child.Generation != 2 {
				t.Fatalf("legacy generation reset: %+v %v", child, err)
			}
			registration, err := f.registry.GetByID(ctx, probeRegistryID1)
			if err != nil {
				t.Fatal(err)
			}
			registration.Enabled = false
			if err := f.registry.Update(ctx, registration, registration.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := store.RenewRuntime(ctx, live); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("disabled runtime renewed: %v", err)
			}
			if _, err := store.AcquireRuntimeConnector(ctx, live); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("disabled runtime connected: %v", err)
			}
			if err := store.ReleaseRuntime(ctx, live); err != nil {
				t.Fatalf("disabled runtime cannot release: %v", err)
			}
			registration.Enabled = true
			if err := f.registry.Update(ctx, registration, registration.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET epoch = ? WHERE probe_id = ?", int64(math.MaxInt64), probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("epoch overflow accepted: %v", err)
			}
		})
	}
}

func TestProbeRuntimeReleaseRollback(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "rollback")
			store := repository.NewProbeConnectorStore(f.db)
			ctx := t.Context()
			live, err := store.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.AcquireRuntimeConnector(ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetConnectorConnected(ctx, child, true); err != nil {
				t.Fatal(err)
			}
			trigger := "CREATE TRIGGER fail_runtime_child BEFORE UPDATE ON probe_sessions BEGIN SELECT RAISE(ABORT,'child invalidation fault'); END"
			if engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_runtime_child BEFORE UPDATE ON probe_sessions FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='child invalidation fault'"
			}
			if _, err := f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.db.ExecContext(t.Context(), "DROP TRIGGER IF EXISTS fail_runtime_child") })
			if err := store.ReleaseRuntime(ctx, live); err == nil {
				t.Fatal("partial release reported success")
			}
			var owner string
			if err := f.db.NewRaw("SELECT owner_id FROM probe_runtime_owners WHERE probe_id = ?", live.ProbeID).Scan(ctx, &owner); err != nil || owner != live.OwnerID {
				t.Fatalf("failed child write still released parent: %q %v", owner, err)
			}
			var connected bool
			if err := f.db.NewRaw("SELECT connected FROM probe_sessions WHERE probe_id = ?", live.ProbeID).Scan(ctx, &connected); err != nil || !connected {
				t.Fatalf("failed release changed session: %v %v", connected, err)
			}
			if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_runtime_child"); err != nil {
				t.Fatal(err)
			}
			if err := store.ReleaseRuntime(ctx, live); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProbeRuntimeTakeoverFencesReplay(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := newReplayFixture(t, engine)
			ctx := t.Context()
			store := repository.NewProbeConnectorStore(r.f.db)
			if err := store.ReleaseConnector(ctx, domain.ProbeConnectorLease{ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration}); err != nil {
				t.Fatal(err)
			}
			live, err := store.AcquireRuntime(ctx, r.session.ProbeID, r.session.OwnerID)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.AcquireRuntimeConnector(ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			r.session.ConnectionGeneration = child.Generation
			r.ingest(t, r.batch(r.observation(1)))
			if _, err := r.f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until = 0 WHERE probe_id = ?", live.ProbeID); err != nil {
				t.Fatal(err)
			}
			next, err := store.AcquireRuntime(ctx, live.ProbeID, live.OwnerID)
			if err != nil {
				t.Fatal(err)
			}
			batch := r.batch(r.observation(2))
			if _, err := r.store.IngestReplayBatch(ctx, r.session, batch, &services.AccessService{}); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("old child replay survived takeover: %v", err)
			}
			if n := replayCount(t, r.f, "probe_observations"); n != 1 {
				t.Fatalf("stale replay wrote %d observations", n)
			}
			current, err := store.AcquireRuntimeConnector(ctx, next)
			if err != nil {
				t.Fatal(err)
			}
			r.session.ConnectionGeneration = current.Generation
			r.ingest(t, batch)
			if n := replayCount(t, r.f, "probe_observations"); n != 2 {
				t.Fatalf("new owner did not resume exact cursor: %d", n)
			}
		})
	}
}

func TestProbeRuntimeRenewalCannotShortenExistingDeadline(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "clock-step")
			store := repository.NewProbeConnectorStore(f.db)
			ctx := t.Context()
			live, err := store.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2)
			if err != nil {
				t.Fatal(err)
			}
			child, err := store.AcquireRuntimeConnector(ctx, live)
			if err != nil {
				t.Fatal(err)
			}
			// Model persisted deadlines written before a backward DB clock step.
			// Neither the DB server clock nor the process clock needs to be changed.
			previous := live.LeaseUntil.Add(time.Minute)
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until = ? WHERE probe_id = ?", previous.Unix(), live.ProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = ? WHERE probe_id = ?", previous.Unix()-1, live.ProbeID); err != nil {
				t.Fatal(err)
			}
			renewed, err := store.RenewRuntime(ctx, live)
			if err != nil || renewed.LeaseUntil.Before(previous) {
				t.Fatalf("backward clock shortened parent below existing child: %+v %v", renewed, err)
			}
			if _, err := store.RenewConnector(ctx, child); err != nil {
				t.Fatal(err)
			}
		})
	}
}
