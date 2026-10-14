-- Source-local effective regional state only; no desired-mode backfill.
CREATE TABLE probe_local_regional_applications (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (probe_id = 'local'),
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    generation BIGINT NOT NULL CHECK (generation > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001'),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery = 'regional'),
    PRIMARY KEY (probe_id, config_revision, monitor_id),
    FOREIGN KEY (probe_id, config_revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE CASCADE
) ENGINE=InnoDB;
