package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
)

// TestIssue256NativeClaimDeadlock requires an actual InnoDB victim, not a
// driver-injected error. The heavier writer holds the second candidate while
// ClaimBatch locks the first; requesting the first closes the lock cycle.
func TestIssue256NativeClaimDeadlock(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	first, second := f.monitor(t), f.monitor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	writer := reopenConfigDB(t, f)
	writer.SetMaxOpenConns(1)
	claimDB := reopenConfigDB(t, f)
	claimDB.SetMaxOpenConns(1)
	hook := newDeleteDiagnosticHook()
	attachInPlaceQueryHook(claimDB, hook)
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal("begin contention writer")
	}
	defer func() { _ = tx.Rollback() }()
	var claimID int64
	if err := claimDB.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &claimID); err != nil {
		t.Fatal("read claim connection identity")
	}
	// Give the writer more undo work so InnoDB chooses the read-only claim as
	// its victim. These rows are outside the bounded first-two candidate batch.
	for i := 0; i < 8; i++ {
		id := f.monitor(t)
		if _, err := tx.ExecContext(ctx, "UPDATE monitors SET check_interval = check_interval + 1 WHERE id = ?", id); err != nil {
			t.Fatal("weight contention writer")
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE monitors SET check_interval = check_interval + 1 WHERE id = ?", second); err != nil {
		t.Fatal("lock second candidate")
	}
	type outcome struct {
		ids []int64
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		rows, err := mariadb.NewMonitorRepo(claimDB).ClaimBatch(ctx, "issue256-native", 2, time.Minute)
		result := outcome{err: err}
		for _, row := range rows {
			result.ids = append(result.ids, row.ID)
		}
		done <- result
	}()
	poll := time.NewTicker(time.Millisecond)
	defer poll.Stop()
	for {
		// A running SELECT is not proof that the first lock has been acquired.
		// Read-only InnoDB transactions may be absent from INNODB_LOCK_WAITS;
		// inspect this connection's actual lock wait without logging SQL/data.
		var kind, name, status string
		if err := f.db.QueryRowContext(ctx, "SHOW ENGINE INNODB STATUS").Scan(&kind, &name, &status); err != nil {
			t.Fatal("observe candidate lock wait")
		}
		waiting := false
		for _, block := range strings.Split(status, "---TRANSACTION") {
			if strings.Contains(block, fmt.Sprintf("MariaDB thread id %d,", claimID)) && strings.Contains(block, "LOCK WAIT") && strings.Contains(block, "monitors") {
				waiting = true
				break
			}
		}
		if waiting {
			break
		}
		select {
		case result := <-done:
			var engineError *mysql.MySQLError
			if errors.As(result.err, &engineError) {
				t.Fatalf("claim completed before lock wait: errno=%d", engineError.Number)
			}
			t.Fatalf("claim completed before lock wait: candidates=%d error_type=%T", len(result.ids), result.err)
		case <-ctx.Done():
			t.Fatal("claim did not wait for second candidate")
		case <-poll.C:
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE monitors SET check_interval = check_interval + 1 WHERE id = ?", first); err != nil {
		t.Fatal("writer did not survive intended claim deadlock")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal("commit contention writer")
	}
	select {
	case result := <-done:
		if result.err != nil || len(result.ids) != 2 || result.ids[0] != first || result.ids[1] != second {
			t.Fatal("claim retry did not return exactly the committed candidate batch")
		}
	case <-ctx.Done():
		t.Fatal("claim retry did not finish")
	}
	hook.mu.Lock()
	victims := 0
	for _, failure := range hook.failures {
		if failure.errno == 1213 {
			victims++
		}
	}
	hook.mu.Unlock()
	if victims != 1 {
		t.Fatalf("native claim deadlock victims=%d, want 1", victims)
	}
	for _, id := range []int64{first, second} {
		if got := leaseEpochOf(t, f, id); got != 1 {
			t.Fatalf("committed lease epoch=%d, want exactly 1", got)
		}
	}
}

// TestIssue256NativeSnapshotConflict establishes native 1020 behavior without
// asserting that ClaimBatch's locking-first transaction can reach this state.
func TestIssue256NativeSnapshotConflict(t *testing.T) {
	f := newProbeRegistryFixture(t, "mariadb")
	id := f.monitor(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	peer := reopenConfigDB(t, f)
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		t.Fatal("begin stale snapshot")
	}
	defer func() { _ = tx.Rollback() }()
	// Control the test connection only; do not depend on the server default.
	if _, err := tx.ExecContext(ctx, "SET SESSION innodb_snapshot_isolation = ON"); err != nil {
		t.Fatal("enable snapshot conflict semantics on test connection")
	}
	var epoch int64
	if err := tx.NewRaw("SELECT lease_epoch FROM monitors WHERE id = ?", id).Scan(ctx, &epoch); err != nil {
		t.Fatal("establish stale snapshot")
	}
	if _, err := peer.ExecContext(ctx, "UPDATE monitors SET lease_epoch = lease_epoch + 1 WHERE id = ?", id); err != nil {
		t.Fatal("advance snapshot row")
	}
	err = tx.NewRaw("SELECT lease_epoch FROM monitors WHERE id = ? FOR UPDATE", id).Scan(ctx, &epoch)
	var conflict *mysql.MySQLError
	if !errors.As(err, &conflict) || conflict.Number != 1020 {
		t.Fatal("stale snapshot locking read did not return native 1020")
	}
	if got := leaseEpochOf(t, f, id); got != 1 {
		t.Fatalf("snapshot conflict changed committed epoch: %d", got)
	}
}
