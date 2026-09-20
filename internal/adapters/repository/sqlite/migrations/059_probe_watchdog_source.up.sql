-- Stop all writers. Preserve existing source IDs, immutable context and leases.
CREATE TABLE IF NOT EXISTS probe_delivery_intents_v059 (
    delivery_id TEXT NOT NULL PRIMARY KEY,
    source_alert_id TEXT NOT NULL,
    source_transition_version INTEGER NOT NULL CHECK (typeof(source_transition_version) = 'integer' AND source_transition_version >= 1),
    probe_id TEXT NOT NULL,
    notification_id INTEGER NOT NULL CHECK (typeof(notification_id) = 'integer' AND notification_id >= 1),
    notification_version INTEGER NOT NULL CHECK (typeof(notification_version) = 'integer' AND notification_version >= 1),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary', 'probe_connection')),
    monitor_id INTEGER,
    assignment_generation INTEGER CHECK (assignment_generation IS NULL OR (typeof(assignment_generation) = 'integer' AND assignment_generation >= 1)),
    stream_id TEXT,
    source_seq INTEGER NOT NULL CHECK (typeof(source_seq) = 'integer' AND source_seq >= 1),
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision >= 1),
    check_status INTEGER NOT NULL CHECK (check_status IN (0, 1)),
    check_output TEXT NOT NULL,
    observed_at TEXT NOT NULL,
    incident_status TEXT NOT NULL CHECK (incident_status IN ('firing', 'resolved')),
    started_at TEXT NOT NULL,
    resolved_at TEXT,
    available_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('pending', 'leased', 'retrying', 'sent', 'failed', 'superseded')),
    attempt INTEGER NOT NULL DEFAULT 0 CHECK (typeof(attempt) = 'integer' AND attempt >= 0),
    lease_token TEXT,
    leased_at TEXT,
    lease_until TEXT,
    error_code TEXT,
    outcome_at TEXT,
    created_at TEXT NOT NULL,
    escalation_policy_id BIGINT NOT NULL DEFAULT 0 CHECK (escalation_policy_id >= 0),
    escalation_step INT NOT NULL DEFAULT 0 CHECK (escalation_step >= 0),
    CHECK ((event_kind = 'probe_connection' AND monitor_id IS NULL AND assignment_generation IS NULL AND stream_id IS NULL AND escalation_policy_id = 0 AND escalation_step = 0) OR
           (event_kind <> 'probe_connection' AND monitor_id IS NOT NULL AND monitor_id > 0 AND assignment_generation IS NOT NULL AND assignment_generation > 0 AND stream_id IS NOT NULL)),
    CHECK ((incident_status = 'firing' AND resolved_at IS NULL AND check_status = 0) OR
           (incident_status = 'resolved' AND resolved_at IS NOT NULL AND check_status = 1)),
    CHECK ((status = 'pending' AND attempt = 0 AND lease_token IS NULL AND leased_at IS NULL AND lease_until IS NULL) OR
           (status = 'leased' AND attempt >= 1 AND lease_token IS NOT NULL AND leased_at IS NOT NULL AND lease_until IS NOT NULL AND lease_until > leased_at) OR
           (status IN ('retrying', 'sent', 'failed') AND attempt >= 1 AND lease_token IS NOT NULL AND leased_at IS NOT NULL AND lease_until IS NULL AND outcome_at IS NOT NULL) OR
           (status = 'superseded' AND lease_until IS NULL AND outcome_at IS NOT NULL AND
               ((attempt = 0 AND lease_token IS NULL AND leased_at IS NULL) OR
                (attempt >= 1 AND lease_token IS NOT NULL AND leased_at IS NOT NULL)))),
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
INSERT INTO probe_delivery_intents_v059 (delivery_id, source_alert_id, source_transition_version, probe_id, notification_id, notification_version, event_kind, monitor_id, assignment_generation, stream_id, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at, escalation_policy_id, escalation_step) SELECT delivery_id, source_alert_id, source_transition_version, probe_id, notification_id, notification_version, event_kind, monitor_id, assignment_generation, stream_id, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at, escalation_policy_id, escalation_step FROM probe_delivery_intents;
DROP TABLE probe_delivery_intents;
ALTER TABLE probe_delivery_intents_v059 RENAME TO probe_delivery_intents;
CREATE INDEX idx_probe_delivery_due ON probe_delivery_intents (probe_id, available_at, created_at, delivery_id);

CREATE TABLE IF NOT EXISTS probe_hub_watchdog_incidents (
    source_alert_id TEXT NOT NULL PRIMARY KEY,
    probe_id TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL,
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS probe_watchdog_events (
    probe_id TEXT NOT NULL,
    seq INTEGER NOT NULL CHECK (seq > 0),
    source_alert_id TEXT NOT NULL,
    transition_version INTEGER NOT NULL CHECK (transition_version > 0),
    observed_at TIMESTAMP NOT NULL,
    payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536),
    PRIMARY KEY (probe_id, seq),
    UNIQUE (source_alert_id, transition_version),
    FOREIGN KEY (source_alert_id) REFERENCES probe_hub_watchdog_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS probe_watchdog_state (
    probe_id TEXT NOT NULL PRIMARY KEY,
    version INTEGER NOT NULL CHECK (version > 0),
    config_revision INTEGER NOT NULL CHECK (config_revision > 0),
    armed INTEGER NOT NULL CHECK (armed IN (0,1)),
    loss_elapsed_ns INTEGER NOT NULL CHECK (loss_elapsed_ns >= 0),
    pending_loss INTEGER NOT NULL CHECK (pending_loss IN (0,1)),
    status TEXT NOT NULL CHECK (status IN ('unarmed','starting','healthy','suspect','lost','recovering')),
    source_alert_id TEXT NULL,
    incident_seq INTEGER NOT NULL CHECK (incident_seq >= 0),
    last_enqueued_at TIMESTAMP NULL,
    updated_at TIMESTAMP NOT NULL,
    CHECK (armed = 1 OR (loss_elapsed_ns = 0 AND pending_loss = 0 AND status = 'unarmed')),
    CHECK ((source_alert_id IS NULL AND incident_seq = 0) OR (source_alert_id IS NOT NULL AND incident_seq > 0)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT,
    FOREIGN KEY (source_alert_id) REFERENCES probe_hub_watchdog_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id,config_revision) REFERENCES probe_config_snapshots(probe_id,revision) ON DELETE RESTRICT
);
