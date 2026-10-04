-- Regional heartbeat identity and per-probe rollup uniqueness.
-- SQLite cannot DROP a table UNIQUE constraint, so rollup tables are rebuilt
-- while preserving row ids. Heartbeats stay additive (no unique source-event
-- key here; dedup lives in probe_streams).

ALTER TABLE heartbeats ADD COLUMN probe_id TEXT NOT NULL DEFAULT 'local';
ALTER TABLE heartbeats ADD COLUMN stream_id TEXT;
ALTER TABLE heartbeats ADD COLUMN source_seq INTEGER;
ALTER TABLE heartbeats ADD COLUMN assignment_generation INTEGER NOT NULL DEFAULT 1;
ALTER TABLE heartbeats ADD COLUMN received_at TEXT;
ALTER TABLE heartbeats ADD COLUMN config_revision INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_hb_monitor_probe_time
    ON heartbeats (monitor_id, probe_id, time DESC, id DESC);

CREATE TABLE heartbeat_1m_probe (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    probe_id        TEXT NOT NULL DEFAULT 'local',
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    unknown_count   INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, probe_id, bucket)
);
INSERT INTO heartbeat_1m_probe (
    id, monitor_id, probe_id, bucket, up_count, down_count, pending_count, maint_count,
    unknown_count, avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, 'local', bucket, up_count, down_count, pending_count, maint_count,
    0, avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1m;
DROP TABLE heartbeat_1m;
ALTER TABLE heartbeat_1m_probe RENAME TO heartbeat_1m;
CREATE INDEX idx_1m_monitor_bucket ON heartbeat_1m (monitor_id, bucket DESC);
CREATE INDEX idx_1m_monitor_probe_bucket ON heartbeat_1m (monitor_id, probe_id, bucket DESC);

CREATE TABLE heartbeat_1h_probe (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    probe_id        TEXT NOT NULL DEFAULT 'local',
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    unknown_count   INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, probe_id, bucket)
);
INSERT INTO heartbeat_1h_probe (
    id, monitor_id, probe_id, bucket, up_count, down_count, pending_count, maint_count,
    unknown_count, avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, 'local', bucket, up_count, down_count, pending_count, maint_count,
    0, avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1h;
DROP TABLE heartbeat_1h;
ALTER TABLE heartbeat_1h_probe RENAME TO heartbeat_1h;
CREATE INDEX idx_1h_monitor_bucket ON heartbeat_1h (monitor_id, bucket DESC);
CREATE INDEX idx_1h_monitor_probe_bucket ON heartbeat_1h (monitor_id, probe_id, bucket DESC);

CREATE TABLE heartbeat_1d_probe (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    probe_id        TEXT NOT NULL DEFAULT 'local',
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    unknown_count   INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, probe_id, bucket)
);
INSERT INTO heartbeat_1d_probe (
    id, monitor_id, probe_id, bucket, up_count, down_count, pending_count, maint_count,
    unknown_count, avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, 'local', bucket, up_count, down_count, pending_count, maint_count,
    0, avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1d;
DROP TABLE heartbeat_1d;
ALTER TABLE heartbeat_1d_probe RENAME TO heartbeat_1d;
CREATE INDEX idx_1d_monitor_bucket ON heartbeat_1d (monitor_id, bucket DESC);
CREATE INDEX idx_1d_monitor_probe_bucket ON heartbeat_1d (monitor_id, probe_id, bucket DESC);
