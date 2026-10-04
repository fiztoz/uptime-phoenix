CREATE TABLE IF NOT EXISTS probe_streams (
    probe_id TEXT NOT NULL,
    stream_id TEXT NOT NULL,
    committed_seq INTEGER NOT NULL CHECK (committed_seq >= 0),
    retired_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (probe_id, stream_id),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS probe_observations (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL,
    probe_id TEXT NOT NULL,
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation >= 1),
    stream_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    config_revision INTEGER NOT NULL CHECK (config_revision >= 0),
    status INTEGER NOT NULL,
    raw_status INTEGER NOT NULL,
    down_count INTEGER NOT NULL,
    ping INTEGER NOT NULL DEFAULT 0,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    message TEXT NOT NULL DEFAULT '',
    important INTEGER NOT NULL DEFAULT 0,
    observed_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    UNIQUE (stream_id, seq),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

CREATE INDEX IF NOT EXISTS idx_probe_obs_monitor_probe_time
    ON probe_observations (monitor_id, probe_id, observed_at, id);

CREATE TABLE IF NOT EXISTS monitor_probe_state (
    monitor_id INTEGER NOT NULL,
    probe_id TEXT NOT NULL,
    assignment_generation INTEGER NOT NULL CHECK (assignment_generation >= 1),
    stream_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq >= 1),
    config_revision INTEGER NOT NULL CHECK (config_revision >= 0),
    status INTEGER NOT NULL,
    down_count INTEGER NOT NULL,
    observed_at TEXT NOT NULL,
    received_at TEXT NOT NULL,
    last_success_at TEXT,
    PRIMARY KEY (monitor_id, probe_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS probe_commands (
    command_id TEXT PRIMARY KEY,
    probe_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    source_alert_id TEXT,
    assignment_generation INTEGER,
    expires_at TEXT NOT NULL,
    status TEXT NOT NULL,
    remote_confirmed INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS probe_dirty_buckets (
    monitor_id INTEGER NOT NULL,
    probe_id TEXT NOT NULL,
    resolution TEXT NOT NULL,
    bucket TEXT NOT NULL,
    PRIMARY KEY (monitor_id, probe_id, resolution, bucket),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
