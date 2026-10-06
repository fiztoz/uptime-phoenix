package repository_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// bangkok is a fixed +7 offset, so these contracts do not depend on a tzdata
// install or on the developer machine's zone.
var bangkok = time.FixedZone("UTC+7", 7*3600)

// runHeartbeatUTCBoundContract asserts that a window query whose bounds are
// normalized to UTC finds a recent heartbeat on every engine.
func runHeartbeatUTCBoundContract(t *testing.T, factory repositoryFactory) {
	t.Helper()
	ctx := context.Background()
	repos := factory(t)

	user := createUser(t, ctx, repos, "utc-window-owner")
	monitor := createMonitor(t, ctx, repos, user.ID, "utc-window")

	now := time.Now().UTC().Truncate(time.Second)
	if err := repos.heartbeats.Save(ctx, &domain.Heartbeat{
		MonitorID: monitor.ID, Status: domain.StatusUp, Ping: 42, Time: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("Save heartbeat: %v", err)
	}

	rows, err := repos.heartbeats.ListByMonitor(ctx, monitor.ID, now.Add(-time.Hour), now)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("UTC bounds returned %d heartbeats; want 1 (beat at %s, window [%s, %s])",
			len(rows), now.Add(-30*time.Minute).Format(time.RFC3339),
			now.Add(-time.Hour).Format(time.RFC3339), now.Format(time.RFC3339))
	}
}

func TestHeartbeatUTCBoundContract_SQLite(t *testing.T) {
	runHeartbeatUTCBoundContract(t, sqliteFactory)
}

func TestHeartbeatUTCBoundContract_MariaDB(t *testing.T) {
	runHeartbeatUTCBoundContract(t, mariadbFactory)
}

// TestHeartbeatUTCBound_SQLite_DriverNormalizesToUTC explains WHY the SQLite path
// tolerates a local-zoned bound, and pins the reason rather than only the
// behavior.
//
// heartbeats.time is TEXT on SQLite, and the driver serializes a time.Time to UTC
// before writing it: one instant passed as UTC and as UTC+7 produces the identical
// stored string, and the generated WHERE literal comes out the same for both bound
// styles. No shift can occur, because the location is normalized away before the
// comparison -- which is exactly what the MySQL driver does NOT do (see
// TestHeartbeatUTCBound_MariaDB_LocalZonedBoundShiftsSQL).
//
// The comment this replaces asserted the two engines the other way round. The
// adapter's own .UTC() is therefore redundant for correctness on SQLite; it stays
// so both adapters read identically and no caller can come to depend on
// driver-specific forgiveness.
func TestHeartbeatUTCBound_SQLite_DriverNormalizesToUTC(t *testing.T) {
	db, err := sqlite.NewDB("file:" + t.TempDir() + "/utc-normalize.db?cache=shared")
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
		t.Fatalf("run SQLite migrations: %v", err)
	}
	repo := sqlite.NewRepository(db)
	ctx := context.Background()

	// heartbeats.monitor_id is a foreign key, so the row needs a real parent.
	repos := sqliteRepositorySet(repo)
	user := createUser(t, ctx, repos, "utc-normalize-owner")
	monitor := createMonitor(t, ctx, repos, user.ID, "utc-normalize")

	stamp := time.Date(2026, 3, 14, 6, 30, 0, 0, time.UTC)
	stored := map[string]string{}
	for label, v := range map[string]time.Time{"utc": stamp, "utc+7": stamp.In(bangkok)} {
		if _, err := db.NewRaw(`DELETE FROM heartbeats WHERE monitor_id = ?`, monitor.ID).Exec(ctx); err != nil {
			t.Fatalf("clear %s: %v", label, err)
		}
		if _, err := db.NewRaw(
			`INSERT INTO heartbeats (monitor_id, status, time, ping) VALUES (?, ?, ?, ?)`,
			monitor.ID, 0, v, 5).Exec(ctx); err != nil {
			t.Fatalf("insert %s: %v", label, err)
		}
		var got string
		if err := db.NewRaw(`SELECT time FROM heartbeats WHERE monitor_id = ? LIMIT 1`, monitor.ID).Scan(ctx, &got); err != nil {
			t.Fatalf("scan %s: %v", label, err)
		}
		stored[label] = got
	}

	// The whole explanation in one comparison: one instant, two locations, one
	// stored representation.
	if stored["utc"] != stored["utc+7"] {
		t.Errorf("SQLite serialized the two locations differently: utc=%q utc+7=%q — the driver no "+
			"longer normalizes to UTC, so this engine's tolerance of a zoned bound has ended and the "+
			"two adapters are no longer equivalent", stored["utc"], stored["utc+7"])
	}
	if !strings.HasSuffix(stored["utc"], "+00:00") {
		t.Errorf("stored text %q carries no UTC offset; expected the driver to serialize as +00:00", stored["utc"])
	}
}

// TestHeartbeatUTCBound_MariaDB_LocalZonedBoundShiftsSQL measures the AGENTS.md
// rule 6 mechanism against a real InnoDB TIMESTAMP column, on the same query path
// production uses, and then shows the adapter's normalization rescuing it.
//
// Three facts, in order:
//
//  1. A bun `Where("time >= ?", bound)` renders the bound as *its own wall-clock*,
//     so a UTC+7 bound for the same instant is emitted seven hours later and
//     matches nothing. This is the permanently-blank 1h/3h/6h chart.
//  2. `loc=UTC` in the DSN does not prevent fact 1. The mariadb adapter comment
//     claims the driver "does convert to the DSN's loc=UTC, so this is
//     belt-and-braces here"; that claim is wrong on this path, and this test is
//     the refutation. The explicit .UTC() is load-bearing.
//  3. Routing the identical local-zoned bound through HeartbeatRepo.ListByMonitor
//     returns the row, because the adapter normalizes. So the defense is real,
//     and it is the normalization doing the work — not the DSN.
//
// MariaDB-only on purpose: SQLite stores the zone in the serialized value and does
// not exhibit the shift, so asserting a shift there would encode wrong engine
// semantics (AGENTS.md rules 6 and 12).
func TestHeartbeatUTCBound_MariaDB_LocalZonedBoundShiftsSQL(t *testing.T) {
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB wall-clock mechanism check")
	}
	validateMariaDBDSN(t, dsn)
	ctx := context.Background()

	db, err := mariadb.NewDB(dsn)
	if err != nil {
		t.Fatalf("open MariaDB from TEST_MARIADB_DSN: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := repository.RunMigrations(db.DB, "mariadb"); err != nil {
		t.Fatalf("run MariaDB migrations: %v", err)
	}
	healMariaDBTail(t, db)
	resetMariaDB(t, db.DB)

	repo := mariadb.NewRepository(db)
	repos := mariadbRepositorySet(repo)
	user := createUser(t, ctx, repos, "utc-mechanism-owner")
	monitor := createMonitor(t, ctx, repos, user.ID, "utc-mechanism")

	now := time.Now().UTC().Truncate(time.Second)
	if err := repo.HeartbeatRepo.Save(ctx, &domain.Heartbeat{
		MonitorID: monitor.ID, Status: domain.StatusUp, Ping: 7, Time: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatalf("Save heartbeat: %v", err)
	}

	// Guard the premise: the two windows below denote identical instants, so any
	// difference in their result sets can only come from the bound's location.
	utcFrom, utcTo := now.Add(-time.Hour), now
	localFrom, localTo := utcFrom.In(bangkok), utcTo.In(bangkok)
	if !localFrom.Equal(utcFrom) || !localTo.Equal(utcTo) {
		t.Fatalf("local bounds do not denote the same instants as the UTC bounds")
	}

	// (1) Raw bun query, unnormalized local-zoned bound: the hazard.
	countRaw := func(from, to time.Time) int {
		t.Helper()
		n, err := db.NewSelect().Model(&repository.HeartbeatModel{}).
			Where("monitor_id = ?", monitor.ID).
			Where("time >= ?", from).
			Where("time <= ?", to).
			Count(ctx)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return int(n)
	}
	if got := countRaw(utcFrom, utcTo); got != 1 {
		t.Fatalf("raw UTC-bounded window returned %d rows; want 1 — fixture is broken", got)
	}
	if got := countRaw(localFrom, localTo); got != 0 {
		t.Errorf("raw local-zoned window returned %d rows; want 0 — the driver no longer writes "+
			"a local-zoned bound as its local wall-clock. The AGENTS.md rule 6 hazard has changed "+
			"shape; revisit this contract and the adapter comments.", got)
	}

	// (3) Same local-zoned bound through the adapter, which normalizes.
	rows, err := repo.HeartbeatRepo.ListByMonitor(ctx, monitor.ID, localFrom, localTo)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("adapter returned %d rows for a local-zoned bound; want 1 — HeartbeatRepo."+
			"ListByMonitor must keep forcing its bounds to UTC", len(rows))
	}
}
