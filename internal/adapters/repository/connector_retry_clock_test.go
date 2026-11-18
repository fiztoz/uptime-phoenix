package repository_test

import (
	"database/sql/driver"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

// clockObservedRows reads real engine rows. Its test-only transform can supply
// explicitly synthetic distinct seconds to test which attempt's value is used.
type clockObservedRows struct {
	driver.Rows
	observe func(driver.Value) driver.Value
}

func (r *clockObservedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		dest[0] = r.observe(dest[0])
	}
	return err
}

func TestConnectorRetryResamplesDatabaseClock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, controlled := range []bool{false, true} {
				name := "actual-clock"
				if controlled {
					name = "controlled-distinct-clock"
				}
				t.Run(name, func(t *testing.T) {
					f := newProbeRegistryFixture(t, engine)
					f.remote(t, probeRegistryID1, "retry-clock")
					attempts, writes := 0, 0
					samples := map[int][]int64{}
					delivered := map[int][]int64{}
					var firstActual int64
					connector := &issue256Connector{stripForUpdate: engine == "sqlite"}
					connector.fault = func(query string) error {
						if strings.HasPrefix(query, "UPDATE probes SET id = id") {
							attempts++
						}
						if strings.HasPrefix(query, "INSERT INTO \"probe_runtime_owners\"") || strings.HasPrefix(query, "INSERT INTO `probe_runtime_owners`") {
							writes++
							if writes == 1 {
								return &mysql.MySQLError{Number: 1213}
							}
						}
						return nil
					}
					connector.observeRows = func(query string, rows driver.Rows) driver.Rows {
						if query != "SELECT unixepoch()" && query != "SELECT UNIX_TIMESTAMP()" {
							return rows
						}
						attempt := attempts
						return &clockObservedRows{Rows: rows, observe: func(v driver.Value) driver.Value {
							var seconds int64
							var err error
							switch value := v.(type) {
							case int64:
								seconds = value
							case []byte:
								seconds, err = strconv.ParseInt(string(value), 10, 64)
							case string:
								seconds, err = strconv.ParseInt(value, 10, 64)
							default:
								t.Errorf("unsupported DB clock type %T", v)
								return v
							}
							if err != nil {
								t.Errorf("DB clock decode: %v", err)
								return v
							}
							samples[attempt] = append(samples[attempt], seconds)
							if firstActual == 0 {
								firstActual = seconds
							}
							output := seconds
							if controlled {
								output = firstActual + int64(attempt-1)*17
							}
							delivered[attempt] = append(delivered[attempt], output)
							if controlled {
								return output
							}
							return v
						}}
					}
					db := issue256LeaseDBConnector(t, f, connector)
					got, err := repository.NewProbeConnectorStore(db).AcquireRuntime(t.Context(), probeRegistryID1, probeRegistryID2)
					if err != nil {
						t.Fatal(err)
					}
					if attempts != 2 || writes != 2 {
						t.Fatalf("attempts=%d writes=%d want2/2", attempts, writes)
					}
					if len(samples) != 2 || len(samples[1]) != 1 || len(samples[2]) != 1 {
						t.Fatalf("require one real DB sample per attempt: %v", samples)
					}
					// Real seconds may match. The controlled subcase deliberately makes
					// delivered values distinct, proving use rather than merely rereading.
					if controlled && delivered[1][0] == delivered[2][0] {
						t.Fatal("controlled clock values must differ")
					}
					want := delivered[2][0] + 60
					if samples[1][0] <= 0 || samples[2][0] <= 0 || got.LeaseUntil.Unix() != want || got.Epoch != 1 {
						t.Fatalf("lease=%+v actual=%v delivered=%v want deadline=%d", got, samples, delivered, want)
					}
					var persisted struct {
						Epoch      int64
						LeaseUntil int64
					}
					if err := f.db.NewRaw("SELECT epoch,lease_until FROM probe_runtime_owners WHERE probe_id=?", probeRegistryID1).Scan(t.Context(), &persisted); err != nil {
						t.Fatal(err)
					}
					if persisted.Epoch != 1 || persisted.LeaseUntil != want {
						t.Fatalf("persisted=%+v want deadline=%d", persisted, want)
					}
					t.Logf("synthetic=%t actual DB samples=%v delivered clock samples=%v committed deadline=%d", controlled, samples, delivered, persisted.LeaseUntil)
				})
			}
		})
	}
}
