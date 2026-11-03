package repository

import (
	"context"
	"fmt"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Chart aggregation shared by both engines (ports.HeartbeatChartReader). The
// SQL text is identical except for the bucket-key expression, which each
// adapter supplies because the engines disagree on how to read a timestamp as
// epoch seconds (AGENTS.md rule 12):
//
//   - MariaDB stores heartbeats.time as TIMESTAMP. UNIX_TIMESTAMP(time) would
//     re-interpret the stored value through the SESSION time_zone, which is not
//     the zone Go wrote the UTC wall-clock in unless the session happens to be
//     UTC. TIMESTAMPDIFF against the epoch literal is wall-clock arithmetic in
//     the session zone on both sides, so it yields the UTC epoch of the
//     wall-clock Go wrote regardless of the session time_zone.
//   - SQLite stores heartbeats.time as TEXT with a "+00:00" suffix;
//     strftime('%s', ...) parses it (fraction included) to epoch seconds.
//
// Both keys are floor(epoch / width), the same bucket start Go's
// time.Truncate(width) gives, so the SQL and in-memory paths agree.

// MariaDBChartBucketKey returns the MariaDB bucket-key expression for width.
func MariaDBChartBucketKey(width time.Duration) string {
	return fmt.Sprintf("TIMESTAMPDIFF(SECOND, '1970-01-01 00:00:00', time) DIV %d", chartBucketSeconds(width))
}

// SQLiteChartBucketKey returns the SQLite bucket-key expression for width.
func SQLiteChartBucketKey(width time.Duration) string {
	return fmt.Sprintf("CAST(strftime('%%s', time) AS INTEGER) / %d", chartBucketSeconds(width))
}

func chartBucketSeconds(width time.Duration) int64 {
	s := int64(width / time.Second)
	if s < 1 {
		return 1
	}
	return s
}

// latencyBucketRow is one GROUP BY row of ListLatencyBuckets.
type latencyBucketRow struct {
	BucketKey int64 `bun:"bucket_key"`
	MinPing   int   `bun:"min_ping"`
	MaxPing   int   `bun:"max_ping"`
	PingSum   int64 `bun:"ping_sum"`
	PingCount int64 `bun:"ping_count"`
}

// ListLatencyBuckets aggregates min/max/sum/count ping per bucket over
// [from, to] for one monitor. keyExpr is MariaDBChartBucketKey or
// SQLiteChartBucketKey for the same width. Bounds are forced to UTC (rule 6).
func ListLatencyBuckets(
	ctx context.Context, db bun.IDB, keyExpr string, width time.Duration,
	monitorID int64, from, to time.Time,
) ([]ports.LatencyBucket, error) {
	var rows []latencyBucketRow
	query := `SELECT ` + keyExpr + ` AS bucket_key,
		MIN(ping) AS min_ping, MAX(ping) AS max_ping,
		SUM(ping) AS ping_sum, COUNT(*) AS ping_count
	FROM heartbeats
	WHERE monitor_id = ? AND time >= ? AND time <= ? AND ping > 0
	GROUP BY bucket_key
	ORDER BY bucket_key`
	if err := db.NewRaw(query, monitorID, from.UTC(), to.UTC()).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list latency buckets: %w", err)
	}
	seconds := chartBucketSeconds(width)
	out := make([]ports.LatencyBucket, len(rows))
	for i, r := range rows {
		out[i] = ports.LatencyBucket{
			Start:     time.Unix(r.BucketKey*seconds, 0).UTC(),
			MinPing:   r.MinPing,
			MaxPing:   r.MaxPing,
			PingSum:   r.PingSum,
			PingCount: r.PingCount,
		}
	}
	return out, nil
}

// statusRunRow is one island of ListStatusRuns.
type statusRunRow struct {
	Kind      int       `bun:"kind"`
	StartTime time.Time `bun:"start_time"`
	EndTime   time.Time `bun:"end_time"`
	FirstID   int64     `bun:"first_id"`
}

// statusRunsSQL is a gaps-and-islands query: every heartbeat is classified as
// downtime (DOWN/PENDING), unknown, or other; the difference between its
// position in the whole window and its position within its class is constant
// across a run of consecutive same-class beats, so GROUP BY (kind, grp) yields
// one row per run. Ordering is (time, id) everywhere because heartbeats.time is
// second-precision on MariaDB (rule 8). The global row number is never smaller
// than the per-class one, so the subtraction cannot underflow MariaDB's
// unsigned ROW_NUMBER. Window functions need MariaDB >= 10.2 and SQLite >= 3.25.
const statusRunsSQL = `SELECT kind, MIN(time) AS start_time, MAX(time) AS end_time, MIN(id) AS first_id
FROM (
	SELECT id, time, kind,
		ROW_NUMBER() OVER (ORDER BY time, id) -
			ROW_NUMBER() OVER (PARTITION BY kind ORDER BY time, id) AS grp
	FROM (
		SELECT id, time,
			CASE WHEN status IN (?, ?) THEN ? WHEN status = ? THEN ? ELSE 0 END AS kind
		FROM heartbeats
		WHERE monitor_id = ? AND time >= ? AND time <= ?
	) AS classified
) AS islands
WHERE kind <> 0
GROUP BY kind, grp
ORDER BY start_time DESC, first_id DESC`

// ListStatusRuns returns the newest limit downtime/unknown runs in [from, to],
// ascending by start. limit <= 0 returns every run. Bounds are forced to UTC.
func ListStatusRuns(
	ctx context.Context, db bun.IDB, monitorID int64, from, to time.Time, limit int,
) ([]ports.StatusRun, error) {
	query := statusRunsSQL
	args := []any{
		int(domain.StatusDown), int(domain.StatusPending), int(ports.StatusRunDowntime),
		int(domain.StatusUnknown), int(ports.StatusRunUnknown),
		monitorID, from.UTC(), to.UTC(),
	}
	if limit > 0 {
		query += "\nLIMIT ?"
		args = append(args, limit)
	}
	var rows []statusRunRow
	if err := db.NewRaw(query, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("list status runs: %w", err)
	}
	out := make([]ports.StatusRun, len(rows))
	for i, r := range rows {
		// Rows arrive newest first so LIMIT keeps the newest runs; flip to
		// ascending, the order the charts draw in.
		out[len(rows)-1-i] = ports.StatusRun{
			Kind:  ports.StatusRunKind(r.Kind),
			Start: r.StartTime.UTC(),
			End:   r.EndTime.UTC(),
		}
	}
	return out, nil
}
