-- 1. Durable source-owned evaluated auxiliary conditions. Promotion (two-sample
-- confirmation and warning recovery hysteresis) happens at the source; the
-- version column fences every write like the certificate cursor so a concurrent
-- check forces re-evaluation instead of a lost or duplicated promotion. Row
-- count is bounded by live assignments and pruned with them.
CREATE TABLE edge_condition_state (
    monitor_id INTEGER NOT NULL,
    generation INTEGER NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('session_pool', 'storage')),
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision > 0),
    observed_state TEXT NOT NULL CHECK (observed_state IN ('ok', 'warning', 'error')),
    effective_state TEXT CHECK (effective_state IS NULL OR effective_state IN ('ok', 'warning', 'error')),
    consecutive_state TEXT NOT NULL CHECK (consecutive_state IN ('ok', 'warning', 'error')),
    consecutive_count INTEGER NOT NULL CHECK (typeof(consecutive_count) = 'integer' AND consecutive_count > 0),
    used_value REAL,
    limit_value REAL,
    percent_value REAL,
    threshold_value REAL,
    unit TEXT NOT NULL CHECK (length(unit) <= 256),
    resource TEXT NOT NULL CHECK (length(resource) <= 256),
    scope TEXT NOT NULL CHECK (length(scope) <= 256),
    source TEXT NOT NULL CHECK (length(source) <= 256),
    message TEXT NOT NULL CHECK (length(message) <= 4096),
    observed_at INTEGER NOT NULL,
    stale_after INTEGER NOT NULL CHECK (stale_after >= observed_at),
    last_success_at INTEGER,
    version INTEGER NOT NULL CHECK (typeof(version) = 'integer' AND version >= 0),
    PRIMARY KEY (monitor_id, generation, kind)
);

-- 2. Promoted auxiliary transitions are first-class durable telemetry. SQLite
-- cannot alter a CHECK, so the outbox is rebuilt with the widened kind set,
-- exactly like the watchdog kind was added. Every row is copied; the old table
-- is dropped only after the copy succeeds.
CREATE TABLE edge_telemetry_outbox_replacement (
    seq INTEGER PRIMARY KEY CHECK (typeof(seq) = 'integer' AND seq > 0),
    kind TEXT NOT NULL CHECK (kind IN ('observation', 'alert.transition', 'delivery.result', 'watchdog.transition', 'condition.transition')),
    observed_at INTEGER NOT NULL,
    payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536)
);
INSERT INTO edge_telemetry_outbox_replacement (seq, kind, observed_at, payload) SELECT seq, kind, observed_at, payload FROM edge_telemetry_outbox;
DROP TABLE edge_telemetry_outbox;
ALTER TABLE edge_telemetry_outbox_replacement RENAME TO edge_telemetry_outbox;
CREATE INDEX idx_edge_telemetry_age ON edge_telemetry_outbox (observed_at, seq);
