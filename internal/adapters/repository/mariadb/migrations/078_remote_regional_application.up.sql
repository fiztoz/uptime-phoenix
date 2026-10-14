-- Authenticated remote effective regional reports; no desired-mode backfill.
CREATE TABLE probe_remote_regional_applications (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(probe_id) = 36 AND probe_id <> 'local'),
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(stream_id) = 36 AND stream_id <> '00000000-0000-4000-8000-000000000001'),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery = 'regional'),
    connection_generation BIGINT NOT NULL CHECK (connection_generation > 0),
    recorded_at DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, config_revision, monitor_id, stream_id),
    FOREIGN KEY (probe_id, config_revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT
) ENGINE=InnoDB;
