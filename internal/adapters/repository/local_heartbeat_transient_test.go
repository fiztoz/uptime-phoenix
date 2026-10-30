package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestLocalHeartbeatTransientLockErrorsAreRetryable pins #116: a MariaDB
// deadlock (1213) or lock-wait timeout (1205) aborts the whole local commit,
// so it must surface as ports.ErrStaleLocalState. HeartbeatService retries
// that sentinel from a fresh state read; any other error makes the local
// scheduler log and drop the heartbeat. The trigger raises the real server
// error numbers, so the classification is exercised end to end through the
// driver. MariaDB only: SQLite has no equivalent error numbers.
func TestLocalHeartbeatTransientLockErrorsAreRetryable(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	ctx := context.Background()
	id := localMonitor(t, f)
	for _, tc := range []struct {
		name  string
		errno int
	}{{"Deadlock1213", 1213}, {"LockWaitTimeout1205", 1205}} {
		t.Run(tc.name, func(t *testing.T) {
			trigger := fmt.Sprintf("CREATE TRIGGER transient_local_write BEFORE INSERT ON heartbeats FOR EACH ROW "+
				"SIGNAL SQLSTATE '40001' SET MYSQL_ERRNO = %d, MESSAGE_TEXT = 'injected transient lock failure'", tc.errno)
			if _, err := f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS transient_local_write") })
			hb, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
			if hb != nil {
				t.Fatalf("transient failure returned a heartbeat: %+v", hb)
			}
			if !errors.Is(err, ports.ErrStaleLocalState) {
				t.Fatalf("errno %d: want ErrStaleLocalState so the service retries, got %v", tc.errno, err)
			}
			if got := localSequence(t, f); got != 0 {
				t.Fatalf("transient failure consumed sequence %d", got)
			}
		})
	}
}

// TestLocalHeartbeatNonTransientErrorsStayTerminal guards the other side: an
// ordinary write failure must not be relabelled as retryable contention.
func TestLocalHeartbeatNonTransientErrorsStayTerminal(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			id := localMonitor(t, f)
			trigger := "CREATE TRIGGER fail_local_write BEFORE INSERT ON heartbeats BEGIN SELECT RAISE(ABORT, 'injected local write failure'); END"
			if engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_local_write BEFORE INSERT ON heartbeats FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected local write failure'"
			}
			if _, err := f.db.ExecContext(ctx, trigger); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = f.db.ExecContext(ctx, "DROP TRIGGER IF EXISTS fail_local_write") })
			_, err := f.localHeartbeat.CommitLocalHeartbeat(ctx, localCommit(id, 0))
			if err == nil || errors.Is(err, ports.ErrStaleLocalState) {
				t.Fatalf("ordinary failure must stay terminal, got %v", err)
			}
		})
	}
}
