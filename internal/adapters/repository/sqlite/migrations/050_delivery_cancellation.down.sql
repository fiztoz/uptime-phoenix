-- Stop all writers. The old schema cannot represent cancellation before claim.
CREATE TABLE IF NOT EXISTS delivery_cancellation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO delivery_cancellation_downgrade_guard (ok) SELECT 0 FROM probe_delivery_intents WHERE status = 'superseded' AND attempt = 0 LIMIT 1;
DROP TABLE delivery_cancellation_downgrade_guard;
-- Source-owned availability work. Mirrored outcomes never populate this table.
-- No provider secrets or requests are stored here.
CREATE TABLE IF NOT EXISTS probe_delivery_intents_v049 (
    delivery_id TEXT NOT NULL PRIMARY KEY,
    source_alert_id TEXT NOT NULL,
    source_transition_version INTEGER NOT NULL CHECK (typeof(source_transition_version) = 'integer' AND source_transition_version >= 1),
    probe_id TEXT NOT NULL,
    notification_id INTEGER NOT NULL CHECK (typeof(notification_id) = 'integer' AND notification_id >= 1),
    notification_version INTEGER NOT NULL CHECK (typeof(notification_version) = 'integer' AND notification_version >= 1),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary')),
    monitor_id INTEGER NOT NULL,
    assignment_generation INTEGER NOT NULL CHECK (typeof(assignment_generation) = 'integer' AND assignment_generation >= 1),
    stream_id TEXT NOT NULL,
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
    CHECK ((incident_status = 'firing' AND resolved_at IS NULL AND check_status = 0) OR
           (incident_status = 'resolved' AND resolved_at IS NOT NULL AND check_status = 1)),
    CHECK ((status = 'pending' AND attempt = 0 AND lease_token IS NULL AND leased_at IS NULL AND lease_until IS NULL) OR
           (status = 'leased' AND attempt >= 1 AND lease_token IS NOT NULL AND leased_at IS NOT NULL AND lease_until IS NOT NULL AND lease_until > leased_at) OR
           (status IN ('retrying', 'sent', 'failed', 'superseded') AND attempt >= 1 AND lease_token IS NOT NULL AND leased_at IS NOT NULL AND lease_until IS NULL AND outcome_at IS NOT NULL)),
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
INSERT INTO probe_delivery_intents_v049 SELECT * FROM probe_delivery_intents;
DROP TABLE probe_delivery_intents;
ALTER TABLE probe_delivery_intents_v049 RENAME TO probe_delivery_intents;
CREATE INDEX idx_probe_delivery_due ON probe_delivery_intents (probe_id, available_at, created_at, delivery_id);
