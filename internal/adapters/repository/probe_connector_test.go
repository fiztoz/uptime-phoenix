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
)

func TestProbeConnectorLeaseContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "lease-test")
			var peer *bun.DB
			var err error
			if engine == "sqlite" {
				peer, err = sqlite.NewDB(f.dsn)
			} else {
				peer, err = mariadb.NewDB(f.dsn)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = peer.Close() }()
			first, second := repository.NewProbeConnectorStore(f.db), repository.NewProbeConnectorStore(peer)
			ctx := t.Context()
			// Independent worker connections race for the same initial registration.
			start := make(chan struct{})
			var wg sync.WaitGroup
			leases := make([]domain.ProbeConnectorLease, 2)
			errs := make([]error, 2)
			for n, store := range []*repository.ProbeConnectorStore{first, second} {
				wg.Add(1)
				go func(n int, store *repository.ProbeConnectorStore) {
					defer wg.Done()
					<-start
					leases[n], errs[n] = store.AcquireConnector(ctx, probeRegistryID1, []string{probeRegistryID2, probeRegistryID3}[n])
				}(n, store)
			}
			close(start)
			wg.Wait()
			var live domain.ProbeConnectorLease
			for n, err := range errs {
				if err == nil {
					if live.Generation != 0 {
						t.Fatal("two owners acquired one lease")
					}
					live = leases[n]
				} else if !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("unexpected acquire error: %v", err)
				}
			}
			if live.Generation != 1 || live.LeaseUntil.Location() != time.UTC || live.LeaseUntil.Before(time.Now().UTC().Add(58*time.Second)) {
				t.Fatalf("missing/bad lease: %+v", live)
			}
			if err := first.SetConnectorConnected(ctx, live, true); err != nil {
				t.Fatal(err)
			}
			renewed, err := second.RenewConnector(ctx, live)
			if err != nil || renewed.Generation != live.Generation || !renewed.Connected {
				t.Fatalf("renew changed fence/status: %+v %v", renewed, err)
			}
			newer, err := first.AcquireConnector(ctx, live.ProbeID, live.OwnerID)
			if err != nil || newer.Generation != 2 {
				t.Fatalf("same-owner reconnect reused fence: %+v %v", newer, err)
			}
			if err := first.SetConnectorConnected(ctx, newer, true); err != nil {
				t.Fatal(err)
			}
			for name, mutation := range map[string]func() error{
				"old close":   func() error { return second.SetConnectorConnected(ctx, live, false) },
				"old release": func() error { return second.ReleaseConnector(ctx, live) },
				"old renewal": func() error { _, err := second.RenewConnector(ctx, live); return err },
			} {
				if err := mutation(); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("%s accepted: %v", name, err)
				}
			}
			var connected bool
			if err := f.db.NewRaw("SELECT connected FROM probe_sessions WHERE probe_id = ?", probeRegistryID1).Scan(ctx, &connected); err != nil || !connected {
				t.Fatalf("old callback disconnected replacement: %v %v", connected, err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			if _, err := first.RenewConnector(ctx, newer); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("expired lease revived: %v", err)
			}
			owner := probeRegistryID2
			if owner == live.OwnerID {
				owner = probeRegistryID3
			}
			takeover, err := second.AcquireConnector(ctx, probeRegistryID1, owner)
			if err != nil || takeover.Generation != 3 || takeover.OwnerID == live.OwnerID {
				t.Fatalf("failover did not fence: %+v %v", takeover, err)
			}
			if err := second.ReleaseConnector(ctx, takeover); err != nil {
				t.Fatal(err)
			}
			afterRelease, err := first.AcquireConnector(ctx, probeRegistryID1, live.OwnerID)
			if err != nil || afterRelease.Generation != 4 {
				t.Fatalf("release discarded generation: %+v %v", afterRelease, err)
			}
			probe, err := f.registry.GetByID(ctx, probeRegistryID1)
			if err != nil {
				t.Fatal(err)
			}
			probe.Enabled = false
			if err := f.registry.Update(ctx, probe, probe.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := first.RenewConnector(ctx, afterRelease); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("disabled probe retained authority: %v", err)
			}
			if err := first.ReleaseConnector(ctx, afterRelease); err != nil {
				t.Fatalf("disabled owner could not release: %v", err)
			}
			probe.Enabled = true
			if err := f.registry.Update(ctx, probe, probe.Revision); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET generation = ? WHERE probe_id = ?", int64(math.MaxInt64), probeRegistryID1); err != nil {
				t.Fatal(err)
			}
			if _, err := first.AcquireConnector(ctx, probeRegistryID1, live.OwnerID); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("generation overflow: %v", err)
			}
			if err := runEngineMigration(t, f.db, engine, "052_probe_connector_leases", "down"); err == nil {
				t.Fatal("downgrade discarded a durable fence")
			}
		})
	}
}
