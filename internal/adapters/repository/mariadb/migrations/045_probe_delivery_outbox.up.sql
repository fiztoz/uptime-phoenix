-- Source-owned availability work. Mirrored outcomes never populate this table.
-- Channel versions refer to accepted configuration, not mutable credentials.
CREATE TABLE IF NOT EXISTS probe_delivery_intents (
    delivery_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_transition_version BIGINT NOT NULL CHECK (source_transition_version >= 1),
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_id BIGINT NOT NULL CHECK (notification_id >= 1),
    notification_version BIGINT NOT NULL CHECK (notification_version >= 1),
    event_kind VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (event_kind IN ('status_change', 'incident_summary')),
    monitor_id BIGINT NOT NULL,
    assignment_generation BIGINT NOT NULL CHECK (assignment_generation >= 1),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
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
