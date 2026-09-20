CREATE TABLE IF NOT EXISTS edge_regional_state (
    monitor_id INTEGER NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    seq INTEGER NOT NULL CHECK (typeof(seq) = 'integer' AND seq > 0),
    config_revision INTEGER NOT NULL REFERENCES edge_config(revision),
    status INTEGER NOT NULL CHECK (status IN (0, 1, 2, 3)),
    down_count INTEGER NOT NULL CHECK (down_count >= 0),
    observed_at INTEGER NOT NULL,
    received_at INTEGER NOT NULL,
    last_success_at INTEGER,
    source_alert_id TEXT,
    last_enqueued_at INTEGER,
    PRIMARY KEY (monitor_id, generation)
);

CREATE TABLE IF NOT EXISTS edge_alerts (
    source_alert_id TEXT PRIMARY KEY,
    monitor_id INTEGER NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    status TEXT NOT NULL CHECK (status IN ('firing', 'resolved', 'acked')),
    transition_version INTEGER NOT NULL CHECK (typeof(transition_version) = 'integer' AND transition_version > 0),
    started_at INTEGER NOT NULL,
    resolved_at INTEGER,
    acked_at INTEGER,
    reason TEXT NOT NULL,
    config_revision INTEGER NOT NULL REFERENCES edge_config(revision),
    CHECK ((status = 'resolved' AND resolved_at IS NOT NULL) OR (status <> 'resolved' AND resolved_at IS NULL))
);

CREATE TABLE IF NOT EXISTS edge_telemetry_outbox (
    seq INTEGER PRIMARY KEY CHECK (typeof(seq) = 'integer' AND seq > 0),
    kind TEXT NOT NULL CHECK (kind IN ('observation', 'alert.transition', 'delivery.result')),
    observed_at INTEGER NOT NULL,
    payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536)
);

CREATE TABLE IF NOT EXISTS edge_delivery_outbox (
    delivery_id TEXT PRIMARY KEY,
    source_alert_id TEXT NOT NULL REFERENCES edge_alerts(source_alert_id),
    source_transition_version INTEGER NOT NULL CHECK (source_transition_version > 0),
    notification_id INTEGER NOT NULL CHECK (notification_id > 0),
    notification_version INTEGER NOT NULL CHECK (notification_version > 0),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary')),
    monitor_id INTEGER NOT NULL,
    generation INTEGER NOT NULL CHECK (generation > 0),
    source_seq INTEGER NOT NULL CHECK (source_seq > 0),
    config_revision INTEGER NOT NULL REFERENCES edge_config(revision),
    check_status INTEGER NOT NULL CHECK (check_status IN (0, 1)),
    check_output TEXT NOT NULL,
    observed_at INTEGER NOT NULL,
    incident_status TEXT NOT NULL CHECK (incident_status IN ('firing', 'resolved')),
    started_at INTEGER NOT NULL,
    resolved_at INTEGER,
    available_at INTEGER NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'leased', 'retrying', 'sent', 'failed', 'superseded')),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (typeof(attempt) = 'integer' AND attempt >= 0),
    lease_token TEXT NOT NULL DEFAULT '',
    leased_at INTEGER,
    lease_until INTEGER,
    error_code TEXT NOT NULL DEFAULT '',
    outcome_at INTEGER,
    created_at INTEGER NOT NULL,
    CHECK ((status = 'pending' AND attempt = 0 AND lease_until IS NULL) OR
           (status = 'leased' AND attempt > 0 AND length(lease_token) = 36 AND lease_until > leased_at) OR
           (status IN ('retrying', 'sent', 'failed', 'superseded') AND lease_until IS NULL AND outcome_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS idx_edge_delivery_due ON edge_delivery_outbox (available_at, created_at, delivery_id);
