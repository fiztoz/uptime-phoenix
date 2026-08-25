package repository

import (
	"context"
	"strings"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// latestBatchChunk bounds how many monitor ids go into one UNION ALL statement.
//
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 32766 on modern builds but 999
// on older ones, and MariaDB's max_allowed_packet caps a very long query too.
// Chunking keeps one round trip per 500 monitors instead of one per monitor: at
// 10,000 monitors that is 20 round trips rather than 10,000, which is the whole
// point of this file. Raising it buys little; lowering it is always safe.
const latestBatchChunk = 500

// latestHeartbeatSelect is one GetLatest. ORDER BY/LIMIT live on the inner
// query so they bind per monitor. The derived table is aliased: MariaDB
// rejects the un-aliased form (Error 1064 near UNION ALL). Outer parentheses
// around each UNION arm are a SQLite syntax error (`near "("`), so the arm
// stays an unparenthesized SELECT-from-subquery.
const latestHeartbeatSelect = `SELECT id, monitor_id, status, time, msg, ping, duration, important, down_count FROM (SELECT id, monitor_id, status, time, msg, ping, duration, important, down_count FROM heartbeats WHERE monitor_id = ? ORDER BY time DESC, id DESC LIMIT 1) AS latest_hb`

// latestImportantBeforeSelect is one leading Insights transition: the newest
// important heartbeat strictly before the window, LIMIT 1 per monitor, so the
// query never loads more than one historical row per monitor.
const latestImportantBeforeSelect = `SELECT id, monitor_id, status, time, msg, ping, duration, important, down_count FROM (SELECT id, monitor_id, status, time, msg, ping, duration, important, down_count FROM heartbeats WHERE monitor_id = ? AND important = TRUE AND time < ? ORDER BY time DESC, id DESC LIMIT 1) AS latest_imp`

// latestHeartbeatsUnionSQL builds n GetLatest SELECTs glued with UNION ALL.
func latestHeartbeatsUnionSQL(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	sep := " UNION ALL "
	b.Grow(len(latestHeartbeatSelect)*n + len(sep)*(n-1))
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(latestHeartbeatSelect)
	}
	return b.String()
}

// LatestHeartbeatsForMonitors returns the newest heartbeat for each of the given
// monitor ids, in O(len(ids)/latestBatchChunk) queries rather than O(len(ids)).
//
// Both the SQLite and the MariaDB adapter delegate here so the two engines
// cannot drift. That matters more than the deduplication: the `time DESC,
// id DESC` ordering below is LOAD-BEARING on MariaDB, where heartbeats.time has
// only second precision and same-second rows otherwise come back in engine
// order. Reproducing GetLatest's tie-break exactly is what keeps a monitor that
// just went DOWN from reading back as PENDING (see the GetLatest doc comments).
//
// Each arm is `ORDER BY time DESC, id DESC LIMIT 1`, the same plan GetLatest
// uses on idx_hb_monitor_time. A window function over the full history was
// correct but scanned every monthly partition.
//
// Monitors with no heartbeats are absent from the map; that is not an error.
func LatestHeartbeatsForMonitors(
	ctx context.Context,
	db *bun.DB,
	monitorIDs []int64,
) (map[int64]*domain.Heartbeat, error) {
	out := make(map[int64]*domain.Heartbeat, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}

	// Deduplicate so a caller passing repeats cannot inflate the query.
	seen := make(map[int64]bool, len(monitorIDs))
	ids := make([]int64, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}

	for start := 0; start < len(ids); start += latestBatchChunk {
		end := start + latestBatchChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}

		var models []HeartbeatModel
		err := db.NewRaw(latestHeartbeatsUnionSQL(len(chunk)), args...).Scan(ctx, &models)
		if err != nil {
			return nil, err
		}

		for i := range models {
			m := models[i]
			out[m.MonitorID] = m.ToDomain()
		}
	}

	return out, nil
}

// latestImportantBeforeUnionSQL builds n leading-transition SELECTs glued with
// UNION ALL. Each arm has two placeholders: monitor_id, then the exclusive
// before bound.
func latestImportantBeforeUnionSQL(n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	sep := " UNION ALL "
	b.Grow(len(latestImportantBeforeSelect)*n + len(sep)*(n-1))
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(latestImportantBeforeSelect)
	}
	return b.String()
}

// LatestImportantBeforeForMonitors returns the newest important heartbeat
// strictly before `before` for each monitor, in O(len(ids)/latestBatchChunk)
// queries.
//
// Both adapters delegate here. Each arm is LIMIT 1 on
// idx_hb_monitor_important_time, so the query never loads the full important
// history per monitor. Monitors with no important heartbeat before the bound
// are absent from the map.
func LatestImportantBeforeForMonitors(
	ctx context.Context,
	db *bun.DB,
	monitorIDs []int64,
	before time.Time,
) (map[int64]*domain.Heartbeat, error) {
	out := make(map[int64]*domain.Heartbeat, len(monitorIDs))
	if len(monitorIDs) == 0 {
		return out, nil
	}

	bound := before.UTC()

	seen := make(map[int64]bool, len(monitorIDs))
	ids := make([]int64, 0, len(monitorIDs))
	for _, id := range monitorIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return out, nil
	}

	for start := 0; start < len(ids); start += latestBatchChunk {
		end := start + latestBatchChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		args := make([]any, 0, len(chunk)*2)
		for _, id := range chunk {
			args = append(args, id, bound)
		}

		var models []HeartbeatModel
		err := db.NewRaw(latestImportantBeforeUnionSQL(len(chunk)), args...).Scan(ctx, &models)
		if err != nil {
			return nil, err
		}

		for i := range models {
			m := models[i]
			out[m.MonitorID] = m.ToDomain()
		}
	}

	return out, nil
}
