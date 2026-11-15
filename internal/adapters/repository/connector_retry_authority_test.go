package repository_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Driver-injected1213 verifies transaction rollback and fresh authority reads,
// not native engine locking (covered separately by the real gap deadlock test).
func TestConnectorRetryRechecksAuthority(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, change := range []string{"disabled", "epoch", "expired"} {
				t.Run(change, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					f.remote(t, probeRegistryID1, "retry-authority")
					ctx := t.Context()
					base := repository.NewProbeConnectorStore(f.db)
					lease, err := base.AcquireRuntime(ctx, probeRegistryID1, probeRegistryID2)
					if err != nil {
						t.Fatal(err)
					}
					starts, writes := 0, 0
					db := issue256LeaseDB(t, f, func(query string) error {
						if strings.HasPrefix(query, "UPDATE probes SET id = id") {
							starts++
							if starts == 2 {
								// Prior attempt was rolled back; replacement has not acquired locks.
								var err error
								switch change {
								case "disabled":
									_, err = f.db.ExecContext(ctx, "UPDATE probes SET enabled = ? WHERE id = ?", false, lease.ProbeID)
								case "epoch":
									_, err = f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET epoch = epoch + 1 WHERE probe_id = ?", lease.ProbeID)
								case "expired":
									_, err = f.db.ExecContext(ctx, "UPDATE probe_runtime_owners SET lease_until = 0 WHERE probe_id = ?", lease.ProbeID)
								}
								if err != nil {
									return err
								}
							}
						}
						if strings.HasPrefix(query, "UPDATE \"probe_runtime_owners\"") || strings.HasPrefix(query, "UPDATE `probe_runtime_owners`") {
							writes++
							if writes == 1 {
								return &mysql.MySQLError{Number: 1213}
							}
						}
						return nil
					})
					got, err := repository.NewProbeConnectorStore(db).RenewRuntime(ctx, lease)
					if !errors.Is(err, ports.ErrConflict) || got.Epoch != 0 || got.OwnerID != "" {
						t.Fatalf("stale authority returned: %+v %v", got, err)
					}
					if starts != 2 || writes != 1 {
						t.Fatalf("starts=%d writes=%d want2/1", starts, writes)
					}
				})
			}
		})
	}
}
