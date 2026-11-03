package repository_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// runChartAggregateEquivalenceContract pins AGENTS.md rule 12 for the #195
// chart reader: the SQL aggregation (bucket arithmetic and gaps-and-islands
// runs) must produce exactly what the in-memory chart_aggregate functions
// produce from the same stored rows, on every engine.
func runChartAggregateEquivalenceContract(t *testing.T, repos repositorySet) {
	t.Helper()
	ctx := context.Background()
	reader, ok := repos.heartbeats.(ports.HeartbeatChartReader)
	if !ok {
		t.Fatalf("%T does not implement ports.HeartbeatChartReader", repos.heartbeats)
	}
	user := createUser(t, ctx, repos, "chart-equivalence")
	monitor := createMonitor(t, ctx, repos, user.ID, "chart-equivalence")

	// Fixed, non-aligned instants so bucket edges are exercised; sub-second
	// offsets reach SQLite's fractional text (MariaDB truncates them).
	to := time.Date(2026, 10, 2, 18, 7, 13, 0, time.UTC)
	from := to.Add(-6 * time.Hour)
	statuses := []domain.Status{
		domain.StatusUp, domain.StatusUp, domain.StatusDown, domain.StatusDown, domain.StatusPending,
		domain.StatusUp, domain.StatusUnknown, domain.StatusUnknown, domain.StatusDown, domain.StatusMaintenance,
		domain.StatusPending, domain.StatusUp, domain.StatusUnknown, domain.StatusUp, domain.StatusUp,
	}
	save := func(at time.Time, status domain.Status, ping int) {
		t.Helper()
		if err := repos.heartbeats.Save(ctx, &domain.Heartbeat{MonitorID: monitor.ID, Status: status, Ping: ping, Time: at}); err != nil {
			t.Fatalf("Save heartbeat: %v", err)
		}
	}
	// Outside the window on both sides: must not contribute.
	save(from.Add(-time.Minute), domain.StatusDown, 9999)
	save(to.Add(time.Minute), domain.StatusDown, 9999)
	for k := 0; k < 6*60; k++ {
		at := from.Add(time.Duration(k)*time.Minute + time.Duration(k%3)*250*time.Millisecond)
		status := statuses[(k/4)%len(statuses)]
		ping := 5 + (k*37)%400
		if k%11 == 0 {
			ping = 0 // no latency sample, still a status sample
		}
		save(at, status, ping)
		if k%17 == 0 {
			// A second-precision tie with the same status (rule 8).
			save(at.Truncate(time.Second), status, ping+1)
		}
	}

	rows, err := repos.heartbeats.ListByMonitor(ctx, monitor.ID, from, to)
	if err != nil {
		t.Fatalf("ListByMonitor: %v", err)
	}
	for _, width := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour} {
		latency, err := reader.ListLatencyBuckets(ctx, monitor.ID, from, to, width)
		if err != nil {
			t.Fatalf("ListLatencyBuckets(%s): %v", width, err)
		}
		got := make([]services.ChartBucket, 0, len(latency))
		for _, b := range latency {
			got = append(got, services.ChartBucket{Time: b.Start, Min: b.MinPing, Avg: float64(b.PingSum) / float64(b.PingCount), Max: b.MaxPing})
		}
		if want := services.BucketHeartbeats(rows, width); !reflect.DeepEqual(got, want) {
			t.Fatalf("width %s: SQL buckets differ from Go buckets\n got %d: %+v\nwant %d: %+v", width, len(got), head(got), len(want), head(want))
		}
	}

	runs, err := reader.ListStatusRuns(ctx, monitor.ID, from, to, 0)
	if err != nil {
		t.Fatalf("ListStatusRuns: %v", err)
	}
	var down, unknown []services.DowntimeInterval
	for _, r := range runs {
		iv := services.DowntimeInterval{Start: r.Start, End: r.End}
		if r.Kind == ports.StatusRunDowntime {
			down = append(down, iv)
		} else {
			unknown = append(unknown, iv)
		}
	}
	if want := utcIntervals(services.DetectDowntimeIntervals(rows)); !reflect.DeepEqual(down, want) {
		t.Fatalf("SQL downtime runs differ from Go\n got %d: %+v\nwant %d: %+v", len(down), down, len(want), want)
	}
	if want := utcIntervals(services.DetectUnknownIntervals(rows)); !reflect.DeepEqual(unknown, want) {
		t.Fatalf("SQL unknown runs differ from Go\n got %d: %+v\nwant %d: %+v", len(unknown), unknown, len(want), want)
	}

	// LIMIT keeps the NEWEST runs, returned ascending.
	limited, err := reader.ListStatusRuns(ctx, monitor.ID, from, to, 3)
	if err != nil {
		t.Fatalf("ListStatusRuns limit: %v", err)
	}
	if !reflect.DeepEqual(limited, runs[len(runs)-3:]) {
		t.Fatalf("limited runs = %+v; want the newest three %+v", limited, runs[len(runs)-3:])
	}

	// An empty window is an empty result, not ErrNotFound.
	empty, err := reader.ListLatencyBuckets(ctx, monitor.ID, to.Add(time.Hour), to.Add(2*time.Hour), 15*time.Minute)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty window buckets = %+v, %v; want none and no error", empty, err)
	}
	noRuns, err := reader.ListStatusRuns(ctx, monitor.ID, to.Add(time.Hour), to.Add(2*time.Hour), 10)
	if err != nil || len(noRuns) != 0 {
		t.Fatalf("empty window runs = %+v, %v; want none and no error", noRuns, err)
	}
}

func head(b []services.ChartBucket) []services.ChartBucket {
	if len(b) > 4 {
		return b[:4]
	}
	return b
}

func utcIntervals(in []services.DowntimeInterval) []services.DowntimeInterval {
	var out []services.DowntimeInterval
	for _, iv := range in {
		out = append(out, services.DowntimeInterval{Start: iv.Start.UTC(), End: iv.End.UTC()})
	}
	return out
}

// TestChartAggregateEquivalence runs the contract on SQLite, on MariaDB, and
// on MariaDB through a session whose time_zone is +07:00. UNIX_TIMESTAMP(time)
// would shift every bucket by seven hours relative to the rows ListByMonitor
// reads back through that same session; the TIMESTAMPDIFF bucket key must not.
// The UTC-session subtest alone cannot tell the two expressions apart.
func TestChartAggregateEquivalence(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) { runChartAggregateEquivalenceContract(t, sqliteFactory(t)) })
	t.Run("mariadb", func(t *testing.T) { runChartAggregateEquivalenceContract(t, mariadbFactory(t)) })
	t.Run("mariadb-session-plus7", func(t *testing.T) {
		mariadbFactory(t) // migrates and empties the schema, or skips
		db, err := mariadb.NewDB(os.Getenv("TEST_MARIADB_DSN") + "&time_zone=%27%2B07%3A00%27")
		if err != nil {
			t.Fatalf("open MariaDB with +07:00 session: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		var zone string
		if err := db.NewRaw("SELECT @@session.time_zone").Scan(context.Background(), &zone); err != nil {
			t.Fatalf("read session time_zone: %v", err)
		}
		if zone != "+07:00" {
			t.Fatalf("session time_zone = %q; want +07:00 (the DSN parameter did not apply)", zone)
		}
		runChartAggregateEquivalenceContract(t, mariadbRepositorySet(mariadb.NewRepository(db)))
	})
}
