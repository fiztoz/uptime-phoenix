package repository_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestHeartbeatProbeIDAndRollupUniqueKey(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			bucket := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
			// Restore the real schema sequence: SQLite 037 rebuilds aggregate tables.
			if err := runEngineMigration(t, f.db, f.engine, "057_probe_history_coverage", "down"); err != nil {
				t.Fatal(err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "037_probe_heartbeat", "down"); err != nil {
				t.Fatalf("empty 037 downgrade: %v", err)
			}
			if _, err := f.db.ExecContext(ctx,
				"INSERT INTO heartbeats (monitor_id, status, time, msg, ping, duration, important, down_count) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
				monitorID, int(domain.StatusUp), bucket, "legacy", 12, 12, false, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx,
				"INSERT INTO heartbeat_1m (monitor_id, bucket, up_count, down_count, pending_count, maint_count, avg_ping, min_ping, max_ping, ping_count, total_checks) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				monitorID, bucket, 1, 0, 0, 0, 12.0, 12, 12, 1, 1); err != nil {
				t.Fatal(err)
			}
			var legacyHeartbeatID, legacyRollupID int64
			if err := f.db.NewSelect().Table("heartbeats").Column("id").Where("monitor_id = ?", monitorID).Scan(ctx, &legacyHeartbeatID); err != nil {
				t.Fatal(err)
			}
			if err := f.db.NewSelect().Table("heartbeat_1m").Column("id").Where("monitor_id = ?", monitorID).Scan(ctx, &legacyRollupID); err != nil {
				t.Fatal(err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "037_probe_heartbeat", "up"); err != nil {
				t.Fatalf("037 upgrade: %v", err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "057_probe_history_coverage", "up"); err != nil {
				t.Fatal(err)
			}
			assertLocalBackfill(t, f, monitorID, legacyHeartbeatID, legacyRollupID)
			if f.engine == "mariadb" {
				assertHeartbeatPartitionUnchanged(t, f.db)
			}

			heartbeats := newEngineHeartbeatRepo(f)
			remote := f.remote(t, probeRegistryID1, "asia")
			sameSecond := bucket
			localHB := &domain.Heartbeat{MonitorID: monitorID, Status: domain.StatusUp, Time: sameSecond, Msg: "local", Ping: 8}
			if err := heartbeats.Save(ctx, localHB); err != nil {
				t.Fatal(err)
			}
			remoteHB := &domain.Heartbeat{
				MonitorID: monitorID, ProbeID: remote.ID, AssignmentGeneration: 1,
				Status: domain.StatusDown, Time: sameSecond, Msg: "remote", Ping: 40,
			}
			if err := heartbeats.Save(ctx, remoteHB); err != nil {
				t.Fatal(err)
			}
			if localHB.ID == 0 || remoteHB.ID <= localHB.ID {
				t.Fatalf("heartbeat ids: local=%d remote=%d", localHB.ID, remoteHB.ID)
			}
			listed, err := heartbeats.ListByMonitor(ctx, monitorID, sameSecond.Add(-time.Second), sameSecond.Add(time.Second))
			if err != nil || len(listed) != 3 {
				t.Fatalf("heartbeats: %d %v", len(listed), err)
			}

			localAgg := &ports.Aggregate1m{MonitorID: monitorID, ProbeID: domain.LocalProbeID, Bucket: bucket, UpCount: 2, TotalChecks: 2}
			if err := heartbeats.SaveAggregate1m(ctx, localAgg); err != nil {
				t.Fatal(err)
			}
			var localIDAfterUpsert int64
			if err := f.db.NewSelect().Table("heartbeat_1m").Column("id").
				Where("monitor_id = ? AND probe_id = ?", monitorID, domain.LocalProbeID).Scan(ctx, &localIDAfterUpsert); err != nil {
				t.Fatal(err)
			}
			if localIDAfterUpsert != legacyRollupID {
				t.Fatalf("local rollup id changed: before=%d after=%d", legacyRollupID, localIDAfterUpsert)
			}
			remoteAgg := &ports.Aggregate1m{MonitorID: monitorID, ProbeID: remote.ID, Bucket: bucket, DownCount: 1, UnknownCount: 1, TotalChecks: 2}
			if err := heartbeats.SaveAggregate1m(ctx, remoteAgg); err != nil {
				t.Fatal(err)
			}
			aggs, err := heartbeats.GetAggregate1m(ctx, monitorID, bucket)
			if err != nil || len(aggs) != 2 {
				t.Fatalf("rollups: %d %v", len(aggs), err)
			}
			byProbe := map[string]*ports.Aggregate1m{}
			for _, agg := range aggs {
				byProbe[agg.ProbeID] = agg
			}
			if byProbe[domain.LocalProbeID] == nil || byProbe[remote.ID] == nil {
				t.Fatalf("missing probe rollup: %+v", byProbe)
			}
			var remoteRollupID int64
			if err := f.db.NewSelect().Table("heartbeat_1m").Column("id").
				Where("monitor_id = ? AND probe_id = ?", monitorID, remote.ID).Scan(ctx, &remoteRollupID); err != nil {
				t.Fatal(err)
			}
			if remoteRollupID <= legacyRollupID {
				t.Fatalf("auto-increment: local=%d remote=%d", legacyRollupID, remoteRollupID)
			}

			if err := runEngineMigration(t, f.db, f.engine, "037_probe_heartbeat", "down"); err == nil {
				t.Fatal("downgrade discarded remote heartbeat/rollup rows")
			}
			if _, err := f.db.ExecContext(ctx, "DELETE FROM heartbeats WHERE probe_id <> ?", domain.LocalProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.ExecContext(ctx, "DELETE FROM heartbeat_1m WHERE probe_id <> ?", domain.LocalProbeID); err != nil {
				t.Fatal(err)
			}
			// Restore the real schema sequence: SQLite 037 rebuilds aggregate tables.
			if err := runEngineMigration(t, f.db, f.engine, "057_probe_history_coverage", "down"); err != nil {
				t.Fatal(err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "037_probe_heartbeat", "down"); err != nil {
				t.Fatalf("local-only 037 downgrade: %v", err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "037_probe_heartbeat", "up"); err != nil {
				t.Fatalf("restore 037: %v", err)
			}
			if err := runEngineMigration(t, f.db, f.engine, "057_probe_history_coverage", "up"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertLocalBackfill(t *testing.T, f probeRegistryFixture, monitorID, heartbeatID, rollupID int64) {
	t.Helper()
	ctx := context.Background()
	var probeID string
	if err := f.db.NewSelect().Table("heartbeats").Column("probe_id").Where("id = ?", heartbeatID).Scan(ctx, &probeID); err != nil || probeID != domain.LocalProbeID {
		t.Fatalf("heartbeat backfill: probe_id=%q err=%v", probeID, err)
	}
	var row struct {
		ID      int64  `bun:"id"`
		ProbeID string `bun:"probe_id"`
	}
	if err := f.db.NewSelect().Table("heartbeat_1m").Column("id", "probe_id").
		Where("monitor_id = ?", monitorID).Scan(ctx, &row); err != nil {
		t.Fatal(err)
	}
	if row.ID != rollupID || row.ProbeID != domain.LocalProbeID {
		t.Fatalf("rollup backfill: id=%d probe_id=%q want id=%d local", row.ID, row.ProbeID, rollupID)
	}
}

func assertHeartbeatPartitionUnchanged(t *testing.T, db *bun.DB) {
	t.Helper()
	var tableName, ddl string
	if err := db.QueryRowContext(context.Background(), "SHOW CREATE TABLE heartbeats").Scan(&tableName, &ddl); err != nil {
		t.Fatal(err)
	}
	canonical := strings.ToUpper(strings.ReplaceAll(ddl, "`", ""))
	if !strings.Contains(canonical, "PARTITION BY RANGE (UNIX_TIMESTAMP(TIME))") {
		t.Fatalf("heartbeat partition expression changed: %s", ddl)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (`id`,`time`)") &&
		!strings.Contains(ddl, "PRIMARY KEY (id, time)") {
		t.Fatalf("heartbeat primary key changed: %s", ddl)
	}
}

func newEngineHeartbeatRepo(f probeRegistryFixture) ports.HeartbeatRepository {
	if f.engine == "sqlite" {
		return sqlite.NewHeartbeatRepo(f.db)
	}
	return mariadb.NewHeartbeatRepo(f.db)
}

func runEngineMigration(t *testing.T, db *bun.DB, engine, name, direction string) error {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(engine, "migrations", name+"."+direction+".sql"))
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
		if strings.TrimSpace(statement) == "" {
			continue
		}
		if _, err := db.ExecContext(context.Background(), statement); err != nil {
			return err
		}
	}
	return nil
}
