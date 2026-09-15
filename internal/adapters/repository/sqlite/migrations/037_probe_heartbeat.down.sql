-- A failed guard is intentional: remote heartbeat or rollup rows would be
-- collapsed by restoring UNIQUE (monitor_id, bucket).

CREATE TABLE IF NOT EXISTS probe_heartbeat_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM probe_heartbeat_down_guard;
INSERT INTO probe_heartbeat_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM heartbeats WHERE probe_id <> 'local' OR stream_id IS NOT NULL)
     + (SELECT COUNT(*) FROM heartbeat_1m WHERE probe_id <> 'local')
     + (SELECT COUNT(*) FROM heartbeat_1h WHERE probe_id <> 'local')
     + (SELECT COUNT(*) FROM heartbeat_1d WHERE probe_id <> 'local');

CREATE TABLE heartbeats_old (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id  INTEGER NOT NULL,
    status      INTEGER NOT NULL,
    time        TEXT NOT NULL,
    msg         TEXT,
    ping        INTEGER,
    duration    INTEGER NOT NULL DEFAULT 0,
    important   INTEGER NOT NULL DEFAULT 0,
    down_count  INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
INSERT INTO heartbeats_old (id, monitor_id, status, time, msg, ping, duration, important, down_count)
SELECT id, monitor_id, status, time, msg, ping, duration, important, down_count FROM heartbeats;
DROP TABLE heartbeats;
ALTER TABLE heartbeats_old RENAME TO heartbeats;
CREATE INDEX idx_hb_monitor_time ON heartbeats (monitor_id, time DESC);
CREATE INDEX idx_hb_monitor_important_time ON heartbeats (monitor_id, important, time DESC, id DESC);

CREATE TABLE heartbeat_1m_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);
INSERT INTO heartbeat_1m_old (
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1m;
DROP TABLE heartbeat_1m;
ALTER TABLE heartbeat_1m_old RENAME TO heartbeat_1m;
CREATE INDEX idx_1m_monitor_bucket ON heartbeat_1m (monitor_id, bucket DESC);

CREATE TABLE heartbeat_1h_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);
INSERT INTO heartbeat_1h_old (
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1h;
DROP TABLE heartbeat_1h;
ALTER TABLE heartbeat_1h_old RENAME TO heartbeat_1h;
CREATE INDEX idx_1h_monitor_bucket ON heartbeat_1h (monitor_id, bucket DESC);

CREATE TABLE heartbeat_1d_old (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id      INTEGER NOT NULL,
    bucket          TEXT NOT NULL,
    up_count        INTEGER NOT NULL DEFAULT 0,
    down_count      INTEGER NOT NULL DEFAULT 0,
    pending_count   INTEGER NOT NULL DEFAULT 0,
    maint_count     INTEGER NOT NULL DEFAULT 0,
    avg_ping        REAL,
    min_ping        INTEGER,
    max_ping        INTEGER,
    ping_count      INTEGER NOT NULL DEFAULT 0,
    total_checks    INTEGER NOT NULL DEFAULT 0,
    UNIQUE (monitor_id, bucket)
);
INSERT INTO heartbeat_1d_old (
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
)
SELECT
    id, monitor_id, bucket, up_count, down_count, pending_count, maint_count,
    avg_ping, min_ping, max_ping, ping_count, total_checks
FROM heartbeat_1d;
DROP TABLE heartbeat_1d;
ALTER TABLE heartbeat_1d_old RENAME TO heartbeat_1d;
CREATE INDEX idx_1d_monitor_bucket ON heartbeat_1d (monitor_id, bucket DESC);

DROP TABLE probe_heartbeat_down_guard;
