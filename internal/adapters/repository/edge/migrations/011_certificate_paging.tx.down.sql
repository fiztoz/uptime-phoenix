-- Refuse to discard certificate paging history, cursor or delivery content.
-- A downgrade must never silently delete an open certificate incident, a
-- delivered threshold, or the durable cursor that stops a duplicate alert.
CREATE TABLE edge_cert_alert_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_cert_alert_downgrade_guard SELECT 0 FROM edge_alerts WHERE subject_kind = 'certificate' LIMIT 1;
INSERT INTO edge_cert_alert_downgrade_guard SELECT 0 FROM edge_cert_alert_state LIMIT 1;
INSERT INTO edge_cert_alert_downgrade_guard SELECT 0 FROM edge_delivery_outbox WHERE event_kind = 'certificate_expiry' OR cert_threshold IS NOT NULL OR cert_days_remaining IS NOT NULL OR cert_not_after IS NOT NULL OR cert_issuer <> '' LIMIT 1;
INSERT INTO edge_cert_alert_downgrade_guard SELECT 0 FROM edge_telemetry_outbox WHERE payload LIKE '%"certificate"%' LIMIT 1;
DROP TABLE edge_cert_alert_downgrade_guard;

DROP TABLE edge_cert_alert_state;
DROP TRIGGER edge_alerts_metadata_insert;
DROP TRIGGER edge_alerts_metadata_update;
DROP TRIGGER edge_alerts_metadata_delete;
DROP TRIGGER edge_watchdog_state_metadata_insert;
DROP TRIGGER edge_watchdog_state_metadata_delete;
DROP INDEX idx_edge_alert_retirement;
DROP INDEX idx_edge_alert_config_ref;
DROP INDEX idx_edge_alert_generation_ref;
DROP INDEX idx_edge_delivery_due;
DROP INDEX idx_edge_delivery_retirement;
DROP INDEX idx_edge_delivery_config_ref;
DROP INDEX idx_edge_delivery_alert_ref;
DROP INDEX edge_alerts_certificate_identity;

CREATE TABLE edge_alerts_replacement (
    source_alert_id TEXT PRIMARY KEY,
    scope TEXT NOT NULL DEFAULT 'regional' CHECK (scope IN ('regional', 'probe_connection')),
    subject_kind TEXT NOT NULL DEFAULT 'availability' CHECK (subject_kind IN ('availability', 'watchdog')),
    monitor_id INTEGER,
    generation INTEGER CHECK (generation > 0),
    status TEXT NOT NULL CHECK (status IN ('firing', 'resolved', 'acked')),
    transition_version INTEGER NOT NULL CHECK (typeof(transition_version) = 'integer' AND transition_version > 0),
    started_at INTEGER NOT NULL,
    resolved_at INTEGER,
    acked_at INTEGER,
    ack_command_id TEXT,
    ack_actor_display_name TEXT,
    ack_note TEXT,
    reason TEXT NOT NULL,
    config_revision INTEGER NOT NULL REFERENCES edge_config(revision),
    CHECK ((scope = 'regional' AND subject_kind = 'availability' AND monitor_id IS NOT NULL AND monitor_id > 0 AND generation IS NOT NULL AND generation > 0) OR
           (scope = 'probe_connection' AND subject_kind = 'watchdog' AND monitor_id IS NULL AND generation IS NULL)),
    CHECK ((status = 'resolved' AND resolved_at IS NOT NULL) OR (status <> 'resolved' AND resolved_at IS NULL))
);
INSERT INTO edge_alerts_replacement (source_alert_id, scope, subject_kind, monitor_id, generation, status, transition_version,
 started_at, resolved_at, acked_at, ack_command_id, ack_actor_display_name, ack_note, reason, config_revision)
 SELECT source_alert_id, scope, subject_kind, monitor_id, generation, status, transition_version,
 started_at, resolved_at, acked_at, ack_command_id, ack_actor_display_name, ack_note, reason, config_revision FROM edge_alerts;

CREATE TABLE edge_delivery_outbox_replacement (
    delivery_id TEXT PRIMARY KEY,
    source_alert_id TEXT NOT NULL REFERENCES edge_alerts_replacement(source_alert_id),
    source_transition_version INTEGER NOT NULL CHECK (source_transition_version > 0),
    notification_id INTEGER NOT NULL CHECK (notification_id > 0),
    notification_version INTEGER NOT NULL CHECK (notification_version > 0),
    event_kind TEXT NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary', 'probe_connection')),
    monitor_id INTEGER,
    generation INTEGER CHECK (generation > 0),
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
    CHECK ((event_kind = 'probe_connection' AND monitor_id IS NULL AND generation IS NULL) OR
           (event_kind <> 'probe_connection' AND monitor_id IS NOT NULL AND monitor_id > 0 AND generation IS NOT NULL AND generation > 0)),
    CHECK ((status = 'pending' AND attempt = 0 AND lease_until IS NULL) OR
           (status = 'leased' AND attempt > 0 AND length(lease_token) = 36 AND lease_until > leased_at) OR
           (status IN ('retrying', 'sent', 'failed', 'superseded') AND lease_until IS NULL AND outcome_at IS NOT NULL))
);
INSERT INTO edge_delivery_outbox_replacement (delivery_id, source_alert_id, source_transition_version, notification_id, notification_version,
 event_kind, monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at,
 resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at)
 SELECT delivery_id, source_alert_id, source_transition_version, notification_id, notification_version,
 event_kind, monitor_id, generation, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at,
 resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at FROM edge_delivery_outbox;

CREATE TABLE edge_watchdog_state_replacement (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    version INTEGER NOT NULL CHECK (typeof(version) = 'integer' AND version > 0),
    config_revision INTEGER NOT NULL REFERENCES edge_config(revision),
    armed INTEGER NOT NULL CHECK (armed IN (0, 1)),
    loss_elapsed_ns INTEGER NOT NULL CHECK (typeof(loss_elapsed_ns) = 'integer' AND loss_elapsed_ns >= 0),
    pending_loss INTEGER NOT NULL CHECK (pending_loss IN (0, 1)),
    status TEXT NOT NULL CHECK (status IN ('unarmed', 'starting', 'healthy', 'suspect', 'lost', 'recovering')),
    source_alert_id TEXT REFERENCES edge_alerts_replacement(source_alert_id),
    incident_seq INTEGER NOT NULL CHECK (typeof(incident_seq) = 'integer' AND incident_seq >= 0),
    last_enqueued_at INTEGER,
    updated_at INTEGER NOT NULL,
    CHECK (armed = 1 OR (loss_elapsed_ns = 0 AND pending_loss = 0 AND status = 'unarmed')),
    CHECK ((source_alert_id IS NULL AND incident_seq = 0) OR (source_alert_id IS NOT NULL AND incident_seq > 0))
);
INSERT INTO edge_watchdog_state_replacement SELECT * FROM edge_watchdog_state;

DROP TABLE edge_delivery_outbox;
DROP TABLE edge_watchdog_state;
DROP TABLE edge_alerts;
ALTER TABLE edge_alerts_replacement RENAME TO edge_alerts;
ALTER TABLE edge_delivery_outbox_replacement RENAME TO edge_delivery_outbox;
ALTER TABLE edge_watchdog_state_replacement RENAME TO edge_watchdog_state;

CREATE INDEX idx_edge_alert_retirement ON edge_alerts (status, resolved_at, source_alert_id);
CREATE INDEX idx_edge_alert_config_ref ON edge_alerts (config_revision);
CREATE INDEX idx_edge_alert_generation_ref ON edge_alerts (monitor_id, generation, status);
CREATE INDEX idx_edge_delivery_due ON edge_delivery_outbox (available_at, created_at, delivery_id);
CREATE INDEX idx_edge_delivery_retirement ON edge_delivery_outbox (status, outcome_at, delivery_id);
CREATE INDEX idx_edge_delivery_config_ref ON edge_delivery_outbox (config_revision);
CREATE INDEX idx_edge_delivery_alert_ref ON edge_delivery_outbox (source_alert_id);
CREATE TRIGGER edge_alerts_metadata_insert AFTER INSERT ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_alerts_metadata_update AFTER UPDATE ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024) - (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
 SELECT CASE WHEN ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024) > (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_alerts_metadata_delete AFTER DELETE ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
END;
CREATE TRIGGER edge_watchdog_state_metadata_insert AFTER INSERT ON edge_watchdog_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((512)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_watchdog_state_metadata_delete AFTER DELETE ON edge_watchdog_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (512)) WHERE id = 1;
END;

UPDATE edge_metadata_budget SET used_bytes =
 (SELECT COALESCE(SUM(length(protected_payload) + 512),0) FROM edge_config)
 + (SELECT COALESCE(SUM(256),0) FROM edge_assignments)
 + (SELECT COALESCE(SUM(length(current_observation) + 512),0) FROM edge_regional_state)
 + (SELECT COALESCE(SUM(length(CAST(reason AS BLOB)) + length(CAST(COALESCE(ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(ack_note, '') AS BLOB)) + 1024),0) FROM edge_alerts)
 + (SELECT COALESCE(SUM(512),0) FROM edge_watchdog_state)
 WHERE id = 1;
