package repository_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func reserveThrottle(t *testing.T, repo ports.NotificationThrottleRepository, key domain.NotificationThrottleKey, at time.Time, interval time.Duration, want bool) {
	t.Helper()
	got, err := repo.Reserve(context.Background(), key, at, interval)
	if err != nil || got != want {
		t.Fatalf("reserve %+v: got %v, want %v, err=%v", key, got, want, err)
	}
}

func peerThrottleStore(t *testing.T, f probeRegistryFixture) ports.NotificationThrottleRepository {
	t.Helper()
	if f.engine == "sqlite" {
		db, err := sqlite.NewDB(f.dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return repository.NewNotificationThrottleStore(db)
	}
	db, err := mariadb.NewDB(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return repository.NewNotificationThrottleStore(db)
}

func TestNotificationThrottleContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ScopeRestartAndClock", func(t *testing.T) { testThrottleScope(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentReservations", func(t *testing.T) { testThrottleConcurrency(t, newProbeRegistryFixture(t, engine)) })
			t.Run("Dispatcher", func(t *testing.T) { testThrottleDispatcher(t, newProbeRegistryFixture(t, engine)) })
			t.Run("MigrationAndRollback", func(t *testing.T) { testThrottleMigration(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

func testThrottleScope(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	f.remote(t, probeRegistryID1, "remote")
	r := repository.NewNotificationThrottleStore(f.db)
	local := domain.NotificationThrottleKey{MonitorID: id, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1}
	remote, next, other := local, local, local
	remote.ProbeID, next.AssignmentGeneration, other.MonitorID = probeRegistryID1, 2, f.monitor(t)
	at := time.Date(2026, 9, 17, 20, 15, 30, 123456000, time.FixedZone("UTC+7", 7*3600))
	for _, key := range []domain.NotificationThrottleKey{local, remote, next, other} {
		reserveThrottle(t, r, key, at, 5*time.Minute, true)
	}
	peer := peerThrottleStore(t, f)
	for _, key := range []domain.NotificationThrottleKey{local, remote, next, other} {
		reserveThrottle(t, peer, key, at.Add(4*time.Minute), 5*time.Minute, false)
	}
	// A forced transition during clock rollback cannot move the cursor backward.
	reserveThrottle(t, peer, local, at.Add(-time.Hour), 0, true)
	reserveThrottle(t, peer, local, at.Add(4*time.Minute), 5*time.Minute, false)
	reserveThrottle(t, peer, local, at.Add(5*time.Minute), 5*time.Minute, true)
	var stored time.Time
	if err := f.db.NewSelect().TableExpr("notification_throttles").Column("last_attempt_at").
		Where("monitor_id = ? AND probe_id = ? AND assignment_generation = 1", id, domain.LocalProbeID).Scan(ctx, &stored); err != nil || !stored.Equal(at.Add(5*time.Minute)) {
		t.Fatalf("UTC timestamp round trip: %v err=%v", stored, err)
	}
	if err := r.Clear(ctx, local); err != nil {
		t.Fatal(err)
	}
	reserveThrottle(t, peer, local, at, 5*time.Minute, true)
	for _, key := range []domain.NotificationThrottleKey{remote, next, other} {
		reserveThrottle(t, peer, key, at, 5*time.Minute, false)
	}
	// Assignment replacement retains old cursors but a re-add starts independently.
	if _, err := f.assignments.Replace(ctx, id, 1, []string{probeRegistryID1}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	if _, err := f.assignments.Replace(ctx, id, 2, []string{domain.LocalProbeID}, domain.HealthPolicyAnyDown); err != nil {
		t.Fatal(err)
	}
	for _, key := range []domain.NotificationThrottleKey{
		{MonitorID: 0, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1},
		{MonitorID: id, ProbeID: "", AssignmentGeneration: 1},
		{MonitorID: id, ProbeID: domain.LocalProbeID, AssignmentGeneration: 0},
	} {
		if ok, err := r.Reserve(ctx, key, at, 0); ok || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid scope accepted: %v %v", ok, err)
		}
		if err := r.Clear(ctx, key); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("invalid clear accepted: %v", err)
		}
	}
	if ok, err := r.Reserve(ctx, local, time.Time{}, 0); ok || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero time accepted: %v %v", ok, err)
	}
	if ok, err := r.Reserve(ctx, local, at, -time.Minute); ok || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("negative interval accepted: %v %v", ok, err)
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM monitors WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := f.db.NewSelect().TableExpr("notification_throttles").ColumnExpr("COUNT(*)").Where("monitor_id = ?", id).Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("monitor deletion left throttle rows: %d %v", count, err)
	}
	reserveThrottle(t, peer, other, at, 5*time.Minute, false)
}

func testThrottleConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	key := domain.NotificationThrottleKey{MonitorID: localMonitor(t, f), ProbeID: domain.LocalProbeID, AssignmentGeneration: 1}
	r := repository.NewNotificationThrottleStore(f.db)
	peer := peerThrottleStore(t, f)
	at := time.Now().UTC()
	for round := 0; round < 2; round++ {
		var wg sync.WaitGroup
		results := make(chan bool, 12)
		errorsOut := make(chan error, 12)
		for i := 0; i < 12; i++ {
			wg.Go(func() {
				var store ports.NotificationThrottleRepository = r
				if i%2 == 0 {
					store = peer
				}
				ok, err := store.Reserve(context.Background(), key, at.Add(time.Duration(round)*5*time.Minute), 5*time.Minute)
				results <- ok
				errorsOut <- err
			})
		}
		wg.Wait()
		close(results)
		close(errorsOut)
		for err := range errorsOut {
			if err != nil {
				t.Fatal(err)
			}
		}
		reserved := 0
		for ok := range results {
			if ok {
				reserved++
			}
		}
		if reserved != 1 {
			t.Fatalf("round %d reserved %d attempts; want one", round, reserved)
		}
	}
}

type throttleNotifier struct {
	calls int
	err   error
}

func (n *throttleNotifier) Notify(context.Context, *domain.Monitor, domain.Status, domain.Status) error {
	n.calls++
	return n.err
}

type throttleMaintenance struct{ active bool }

func (m *throttleMaintenance) IsActive(context.Context, int64) (bool, error) { return m.active, nil }

func testThrottleDispatcher(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	key := domain.NotificationThrottleKey{MonitorID: id, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1}
	n := &throttleNotifier{err: errors.New("provider unavailable")}
	m := &throttleMaintenance{}
	r := repository.NewNotificationThrottleStore(f.db)
	d := services.NewNotificationDispatcher(n, m)
	d.SetThrottleRepository(r)
	monitor := &domain.Monitor{ID: id, ResendInterval: 5}
	hb := &domain.Heartbeat{MonitorID: id, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1, Status: domain.StatusDown}
	down := domain.StatusDown
	d.OnHeartbeat(ctx, monitor, hb, nil)
	// A new service AND connection retain attempt backoff, including send failure.
	d = services.NewNotificationDispatcher(n, m)
	d.SetThrottleRepository(peerThrottleStore(t, f))
	d.OnHeartbeat(ctx, monitor, hb, &down)
	if n.calls != 1 {
		t.Fatalf("restart/provider failure bypassed backoff: %d calls", n.calls)
	}
	if err := r.Clear(ctx, key); err != nil {
		t.Fatal(err)
	}
	m.active = true
	d.OnHeartbeat(ctx, monitor, hb, &down)
	m.active = false
	d.OnHeartbeat(ctx, monitor, hb, &down)
	if n.calls != 2 {
		t.Fatalf("maintenance consumed throttle or failed to suppress: %d", n.calls)
	}
	hb.AssignmentGeneration = 2
	d.OnHeartbeat(ctx, monitor, hb, &down)
	if n.calls != 3 {
		t.Fatalf("new generation inherited throttle: %d", n.calls)
	}
	hb.Status = domain.StatusUp
	d.OnHeartbeat(ctx, monitor, hb, &down)
	key2 := key
	key2.AssignmentGeneration = 2
	reserveThrottle(t, r, key2, time.Now().UTC(), 5*time.Minute, true)
	reserveThrottle(t, r, key, time.Now().UTC(), 5*time.Minute, false)
}

func runThrottleMigration(t *testing.T, f probeRegistryFixture, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.engine, "migrations", "043_notification_throttles."+direction+".sql"))
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines[i] = ""
		}
	}
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if strings.TrimSpace(statement) != "" {
			if _, err := f.db.ExecContext(context.Background(), statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func testThrottleMigration(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	id := localMonitor(t, f)
	key := domain.NotificationThrottleKey{MonitorID: id, ProbeID: domain.LocalProbeID, AssignmentGeneration: 1}
	r := repository.NewNotificationThrottleStore(f.db)
	for _, direction := range []string{"down", "up", "up"} {
		if err := runThrottleMigration(t, f, direction); err != nil {
			t.Fatal(err)
		}
	}
	trigger := "CREATE TRIGGER fail_throttle BEFORE UPDATE ON notification_throttles BEGIN SELECT RAISE(ABORT, 'injected'); END"
	if f.engine == "mariadb" {
		trigger = "CREATE TRIGGER fail_throttle BEFORE UPDATE ON notification_throttles FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected'"
	}
	if _, err := f.db.ExecContext(ctx, trigger); err != nil {
		t.Fatal(err)
	}
	if ok, err := r.Reserve(ctx, key, time.Now().UTC(), 0); err == nil || ok {
		t.Fatalf("failed cursor write reported reservation: %v %v", ok, err)
	}
	var count int
	if err := f.db.NewSelect().TableExpr("notification_throttles").ColumnExpr("COUNT(*)").Scan(ctx, &count); err != nil || count != 0 {
		t.Fatalf("failed reservation left placeholder: %d %v", count, err)
	}
	if _, err := f.db.ExecContext(ctx, "DROP TRIGGER fail_throttle"); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	reserveThrottle(t, r, key, at, time.Minute, true)
	if err := runThrottleMigration(t, f, "up"); err != nil {
		t.Fatal(err)
	}
	reserveThrottle(t, r, key, at, time.Minute, false)
	if err := runThrottleMigration(t, f, "down"); err == nil {
		t.Fatal("downgrade discarded live backoff")
	}
	reserveThrottle(t, r, key, at, time.Minute, false)
	if err := r.Clear(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := runThrottleMigration(t, f, "down"); err != nil {
		t.Fatal(err)
	}
	if err := runThrottleMigration(t, f, "up"); err != nil {
		t.Fatal(err)
	}
	reserveThrottle(t, r, key, at, time.Minute, true)
}
