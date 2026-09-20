-- Refuse to discard watchdog checkpoints, probe incidents or ACK metadata.
CREATE TABLE edge_watchdog_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_watchdog_downgrade_guard SELECT 0 FROM edge_watchdog_state LIMIT 1;
INSERT INTO edge_watchdog_downgrade_guard SELECT 0 FROM edge_alerts WHERE scope = 'probe_connection' OR ack_command_id IS NOT NULL OR ack_actor_display_name IS NOT NULL OR ack_note IS NOT NULL LIMIT 1;
INSERT INTO edge_watchdog_downgrade_guard SELECT 0 FROM edge_telemetry_outbox WHERE kind = 'watchdog.transition' LIMIT 1;
DROP TABLE edge_watchdog_downgrade_guard;
DROP TABLE edge_watchdog_state;
CREATE TABLE edge_alerts_replacement (
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
INSERT INTO edge_alerts_replacement (source_alert_id, monitor_id, generation, status, transition_version, started_at, resolved_at, acked_at, reason, config_revision) SELECT source_alert_id, monitor_id, generation, status, transition_version, started_at, resolved_at, acked_at, reason, config_revision FROM edge_alerts;
CREATE TABLE edge_delivery_outbox_replacement (
    delivery_id TEXT PRIMARY KEY,
    source_alert_id TEXT NOT NULL REFERENCES edge_alerts_replacement(source_alert_id),
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
INSERT INTO edge_delivery_outbox_replacement (delivery_id, source_alert_id, source_transition_version, notification_id, notification_version, event_kind, monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at) SELECT delivery_id, source_alert_id, source_transition_version, notification_id, notification_version, event_kind, monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at FROM edge_delivery_outbox;
CREATE TABLE edge_telemetry_outbox_replacement (
    seq INTEGER PRIMARY KEY CHECK (typeof(seq) = 'integer' AND seq > 0),
    kind TEXT NOT NULL CHECK (kind IN ('observation', 'alert.transition', 'delivery.result')),
    observed_at INTEGER NOT NULL,
    payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536)
);
INSERT INTO edge_telemetry_outbox_replacement (seq, kind, observed_at, payload) SELECT seq, kind, observed_at, payload FROM edge_telemetry_outbox;
DROP TABLE edge_delivery_outbox;
DROP TABLE edge_alerts;
DROP TABLE edge_telemetry_outbox;
ALTER TABLE edge_alerts_replacement RENAME TO edge_alerts;
ALTER TABLE edge_delivery_outbox_replacement RENAME TO edge_delivery_outbox;
ALTER TABLE edge_telemetry_outbox_replacement RENAME TO edge_telemetry_outbox;
CREATE INDEX idx_edge_delivery_due ON edge_delivery_outbox (available_at, created_at, delivery_id);
CREATE INDEX idx_edge_telemetry_age ON edge_telemetry_outbox (observed_at, seq);
