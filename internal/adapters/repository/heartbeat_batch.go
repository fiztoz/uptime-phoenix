package repository

import (
	"context"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// latestBatchChunk bounds how many monitor ids go into one IN (...) list.
//
// SQLite's default SQLITE_MAX_VARIABLE_NUMBER is 32766 on modern builds but 999
// on older ones, and MariaDB's max_allowed_packet caps a very long IN list too.
// Chunking keeps one query per 500 monitors instead of one per monitor: at
// 10,000 monitors that is 20 round trips rather than 10,000, which is the whole
// point of this file. Raising it buys little; lowering it is always safe.
const latestBatchChunk = 500

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
// ROW_NUMBER() is used rather than a GROUP BY / MAX(time) join precisely because
// MAX(time) cannot break the same-second tie, and a self-join on (monitor_id,
// time) would return BOTH tied rows. Window functions are available on SQLite
// 3.25+ (modernc.org/sqlite is far newer) and MariaDB 10.2+.
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

	// Deduplicate so a caller passing repeats cannot inflate the IN list.
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

		ranked := db.NewSelect().
			Model((*HeartbeatModel)(nil)).
			ColumnExpr("id, monitor_id, status, time, msg, ping, duration, important, down_count").
			ColumnExpr("ROW_NUMBER() OVER (PARTITION BY monitor_id ORDER BY time DESC, id DESC) AS rn").
			Where("monitor_id IN (?)", bun.List(chunk))

		var models []HeartbeatModel
		err := db.NewSelect().
			With("ranked", ranked).
			TableExpr("ranked").
			Column("id", "monitor_id", "status", "time", "msg", "ping", "duration", "important", "down_count").
			Where("rn = 1").
			Scan(ctx, &models)
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
