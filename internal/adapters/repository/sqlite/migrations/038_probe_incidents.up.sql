-- Regional incident mirrors and delivery outcomes. Existing alerts rows stay
-- monitor-scoped and are not reinterpreted.

CREATE TABLE IF NOT EXISTS probe_incidents (
    hub_incident_id INTEGER PRIMARY KEY AUTOINCREMENT,
    source_alert_id TEXT NOT NULL UNIQUE,
    scope TEXT NOT NULL CHECK (scope IN ('regional', 'probe_connection')),
    monitor_id INTEGER,
    probe_id TEXT NOT NULL,
    assignment_generation INTEGER,
    status TEXT NOT NULL CHECK (status IN ('firing', 'acked', 'resolved')),
    transition_version INTEGER NOT NULL CHECK (typeof(transition_version) = 'integer' AND transition_version >= 1),
    started_at TEXT NOT NULL,
    resolved_at TEXT,
    acked_at TEXT,
    reason TEXT NOT NULL DEFAULT '',
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision >= 1),
    subject_kind TEXT NOT NULL CHECK (subject_kind IN ('availability', 'capacity', 'certificate', 'watchdog')),
    condition_kind TEXT,
    certificate_threshold INTEGER,
    ack_command_id TEXT,
    ack_actor_display_name TEXT,
    ack_note TEXT,
    escalation_policy_id INTEGER,
    escalation_policy_version INTEGER,
    escalation_status TEXT,
    escalation_next_step INTEGER,
    escalation_next_run_at TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_probe_incidents_monitor
    ON probe_incidents (monitor_id, started_at, hub_incident_id);
CREATE INDEX IF NOT EXISTS idx_probe_incidents_probe
    ON probe_incidents (probe_id, started_at);

CREATE TABLE IF NOT EXISTS probe_delivery_events (
    delivery_id TEXT PRIMARY KEY,
    source_alert_id TEXT NOT NULL,
    source_transition_version INTEGER NOT NULL CHECK (typeof(source_transition_version) = 'integer' AND source_transition_version >= 1),
    probe_id TEXT NOT NULL,
    notification_id INTEGER NOT NULL CHECK (typeof(notification_id) = 'integer' AND notification_id >= 1),
    notification_version INTEGER NOT NULL CHECK (typeof(notification_version) = 'integer' AND notification_version >= 1),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change', 'certificate_expiry', 'capacity_condition', 'probe_connection', 'incident_summary')),
    attempt INTEGER NOT NULL CHECK (typeof(attempt) = 'integer' AND attempt >= 0),
    status TEXT NOT NULL CHECK (status IN ('sent', 'retrying', 'failed', 'superseded')),
    error_code TEXT,
    observed_at TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS idx_probe_delivery_incident
    ON probe_delivery_events (source_alert_id, observed_at);
