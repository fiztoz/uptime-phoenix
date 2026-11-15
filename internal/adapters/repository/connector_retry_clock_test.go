package repository_test

import (
	"database/sql/driver"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
)

// clockObservedRows records the real engine result without substituting a clock
// or changing SQL. MariaDB can return numeric text; SQLite returns int64.
type clockObservedRows struct {
	driver.Rows
	observe func(driver.Value)
}

func (r *clockObservedRows) Next(dest []driver.Value) error {
	err := r.Rows.Next(dest)
	if err == nil {
		r.observe(dest[0])
	}
	return err
}

func TestConnectorRetryResamplesDatabaseClock(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "retry-clock")
			attempts, writes := 0, 0
			samples := map[int][]int64{}
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
				return &clockObservedRows{Rows: rows, observe: func(v driver.Value) {
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
						return
					}
					if err != nil {
						t.Errorf("DB clock decode: %v", err)
						return
					}
					samples[attempt] = append(samples[attempt], seconds)
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
			// Seconds may legitimately be identical. Count actual reads, not elapsed
			// wall time, and tie committed authority to the successful attempt's sample.
			want := samples[2][0] + 60
			if samples[1][0] <= 0 || samples[2][0] <= 0 || got.LeaseUntil.Unix() != want || got.Epoch != 1 {
				t.Fatalf("lease=%+v samples=%v", got, samples)
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
			t.Logf("actual DB samples attempt1=%d attempt2=%d committed deadline=%d", samples[1][0], samples[2][0], persisted.LeaseUntil)
		})
	}
}
