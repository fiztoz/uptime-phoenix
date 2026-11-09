package repository_test

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// M6-F: restart/kill at the ownership-takeover boundary. A child hub owner
// acquires a probe's connector lease, marks it connected and keeps renewing
// until it is SIGKILLed at a random moment, sometimes inside the acquire or
// renew transaction. The parent checks that a peer can't take the lease while
// it is live, that it can once the lease expires, that the takeover fence is
// above every generation the dead owner was handed, and that the dead owner's
// lease can no longer renew, disconnect or release the new session.

const (
	killOwnerA = probeRegistryID2
	killOwnerB = probeRegistryID3
)

func TestKillOwnershipTakeoverChild(t *testing.T) {
	if os.Getenv(killChildEnv) == "" {
		t.Skip("child process of TestKillOwnershipTakeover")
	}
	f := reopenKillFixture(t)
	store := repository.NewProbeConnectorStore(f.db)
	ctx := t.Context()
	_, _ = os.Stdout.WriteString("READY\n")
	lease, err := store.AcquireConnector(ctx, probeRegistryID1, killOwnerA)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
		select {}
	}
	_, _ = fmt.Fprintf(os.Stdout, "L %d\n", lease.Generation)
	if err := store.SetConnectorConnected(ctx, lease, true); err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
		select {}
	}
	for {
		if lease, err = store.RenewConnector(ctx, lease); err != nil {
			_, _ = fmt.Fprintf(os.Stdout, "ERR %v\n", err)
			select {}
		}
		_, _ = os.Stdout.WriteString("R\n")
	}
}

func TestKillOwnershipTakeover(t *testing.T) {
	if os.Getenv(killChildEnv) != "" {
		t.Skip("parent only")
	}
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			if engine == "mariadb" && os.Getenv("TEST_MARIADB_DSN") == "" {
				t.Skip("TEST_MARIADB_DSN is unset")
			}
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "kill-takeover")
			peer := repository.NewProbeConnectorStore(f.db)
			ctx := t.Context()
			seed := time.Now().UnixNano()
			rng := rand.New(rand.NewSource(seed))
			t.Logf("random kill seed %d", seed)
			iterations := killIterations()
			var beforeLease, holding int
			var highest int64
			for i := 0; i < iterations; i++ {
				delay := time.Duration(rng.Int63n(int64(30 * time.Millisecond)))
				res := spawnKilledChild(t, "TestKillOwnershipTakeoverChild", f, "random", delay, nil)
				var handed int64
				for _, l := range res.lines {
					if v, ok := strings.CutPrefix(l, "L "); ok {
						handed, _ = strconv.ParseInt(v, 10, 64)
					}
					if strings.HasPrefix(l, "ERR ") {
						t.Fatalf("iter %d: child failed: %v", i, res.lines)
					}
				}
				var gen int64
				var owner string
				var leaseUntil int64
				err := f.db.QueryRowContext(ctx, "SELECT generation, owner_id, lease_until FROM probe_sessions WHERE probe_id=?", probeRegistryID1).Scan(&gen, &owner, &leaseUntil)
				if handed > 0 {
					holding++
					if err != nil || gen != handed || owner != killOwnerA {
						t.Fatalf("iter %d: child was handed gen %d, durable row gen=%d owner=%s err=%v", i, handed, gen, owner, err)
					}
					// The dead owner's lease is still live: no early takeover.
					if _, err := peer.AcquireConnector(ctx, probeRegistryID1, killOwnerB); !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("iter %d: peer took a live lease: %v", i, err)
					}
				} else {
					beforeLease++
				}
				// Let the lease expire, as wall-clock time would.
				if _, err := f.db.ExecContext(ctx, "UPDATE probe_sessions SET lease_until = 0 WHERE probe_id = ?", probeRegistryID1); err != nil {
					t.Fatal(err)
				}
				takeover, err := peer.AcquireConnector(ctx, probeRegistryID1, killOwnerB)
				if err != nil {
					t.Fatalf("iter %d: takeover after expiry: %v", i, err)
				}
				if takeover.Generation <= handed || takeover.Generation <= highest {
					t.Fatalf("iter %d: takeover gen %d not above dead owner's %d / highest %d", i, takeover.Generation, handed, highest)
				}
				highest = takeover.Generation
				if err := peer.SetConnectorConnected(ctx, takeover, true); err != nil {
					t.Fatal(err)
				}
				if handed > 0 {
					dead := domain.ProbeConnectorLease{ProbeID: probeRegistryID1, OwnerID: killOwnerA, Generation: handed}
					for name, mutation := range map[string]func() error{
						"renew":      func() error { _, err := peer.RenewConnector(ctx, dead); return err },
						"disconnect": func() error { return peer.SetConnectorConnected(ctx, dead, false) },
						"release":    func() error { return peer.ReleaseConnector(ctx, dead) },
					} {
						if err := mutation(); !errors.Is(err, ports.ErrConflict) {
							t.Fatalf("iter %d: dead owner %s accepted: %v", i, name, err)
						}
					}
					var connected bool
					if err := f.db.QueryRowContext(ctx, "SELECT connected FROM probe_sessions WHERE probe_id=?", probeRegistryID1).Scan(&connected); err != nil || !connected {
						t.Fatalf("iter %d: dead owner disconnected the new session: %v %v", i, connected, err)
					}
				}
				if err := peer.ReleaseConnector(ctx, takeover); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("%d kills: %d while holding the lease, %d before it was handed out; highest fence %d", iterations, holding, beforeLease, highest)
		})
	}
}
