-- Stop all writers. Preserve existing source IDs, immutable context and leases.
DROP TABLE IF EXISTS probe_delivery_intents_previous;
CREATE TABLE IF NOT EXISTS probe_delivery_intents_v059 (
    delivery_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_transition_version BIGINT NOT NULL CHECK (source_transition_version >= 1),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_id BIGINT NOT NULL CHECK (notification_id >= 1),
    notification_version BIGINT NOT NULL CHECK (notification_version >= 1),
    event_kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary', 'probe_connection')),
    monitor_id BIGINT NULL,
    assignment_generation BIGINT NULL CHECK (assignment_generation >= 1),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    source_seq BIGINT NOT NULL CHECK (source_seq >= 1),
    config_revision BIGINT NOT NULL CHECK (config_revision >= 1),
    check_status INT NOT NULL CHECK (check_status IN (0, 1)),
    check_output TEXT NOT NULL,
    observed_at DATETIME(6) NOT NULL,
    incident_status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (incident_status IN ('firing', 'resolved')),
    started_at DATETIME(6) NOT NULL,
    resolved_at DATETIME(6) NULL,
    available_at DATETIME(6) NOT NULL,
    status VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (status IN ('pending', 'leased', 'retrying', 'sent', 'failed', 'superseded')),
    attempt BIGINT NOT NULL DEFAULT 0 CHECK (attempt >= 0),
    lease_token VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    leased_at DATETIME(6) NULL,
    lease_until DATETIME(6) NULL,
    error_code VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NULL,
    outcome_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
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
    INDEX idx_probe_delivery_due (probe_id, available_at, created_at, delivery_id),
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
INSERT INTO probe_delivery_intents_v059 (delivery_id, source_alert_id, source_transition_version, probe_id, notification_id, notification_version, event_kind, monitor_id, assignment_generation, stream_id, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at, escalation_policy_id, escalation_step) SELECT delivery_id, source_alert_id, source_transition_version, probe_id, notification_id, notification_version, event_kind, monitor_id, assignment_generation, stream_id, source_seq, config_revision, check_status, check_output, observed_at, incident_status, started_at, resolved_at, available_at, status, attempt, lease_token, leased_at, lease_until, error_code, outcome_at, created_at, escalation_policy_id, escalation_step FROM probe_delivery_intents ON DUPLICATE KEY UPDATE delivery_id = VALUES(delivery_id);
RENAME TABLE probe_delivery_intents TO probe_delivery_intents_previous, probe_delivery_intents_v059 TO probe_delivery_intents;
DROP TABLE probe_delivery_intents_previous;

CREATE TABLE IF NOT EXISTS probe_hub_watchdog_incidents (
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at DATETIME(6) NOT NULL,
    FOREIGN KEY (source_alert_id) REFERENCES probe_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS probe_watchdog_events (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    seq BIGINT NOT NULL CHECK (seq > 0),
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transition_version BIGINT NOT NULL CHECK (transition_version > 0),
    observed_at DATETIME(6) NOT NULL,
    payload MEDIUMBLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536),
    PRIMARY KEY (probe_id, seq),
    UNIQUE (source_alert_id, transition_version),
    FOREIGN KEY (source_alert_id) REFERENCES probe_hub_watchdog_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS probe_watchdog_state (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY,
    version BIGINT NOT NULL CHECK (version > 0),
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    armed INTEGER NOT NULL CHECK (armed IN (0,1)),
    loss_elapsed_ns BIGINT NOT NULL CHECK (loss_elapsed_ns >= 0),
    pending_loss INTEGER NOT NULL CHECK (pending_loss IN (0,1)),
    status VARCHAR(16) NOT NULL CHECK (status IN ('unarmed','starting','healthy','suspect','lost','recovering')),
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    incident_seq BIGINT NOT NULL CHECK (incident_seq >= 0),
    last_enqueued_at DATETIME(6) NULL,
    updated_at DATETIME(6) NOT NULL,
    CHECK (armed = 1 OR (loss_elapsed_ns = 0 AND pending_loss = 0 AND status = 'unarmed')),
    CHECK ((source_alert_id IS NULL AND incident_seq = 0) OR (source_alert_id IS NOT NULL AND incident_seq > 0)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT,
    FOREIGN KEY (source_alert_id) REFERENCES probe_hub_watchdog_incidents(source_alert_id) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id,config_revision) REFERENCES probe_config_snapshots(probe_id,revision) ON DELETE RESTRICT
) ENGINE=InnoDB;
