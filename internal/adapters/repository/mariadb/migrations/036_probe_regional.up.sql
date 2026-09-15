CREATE TABLE IF NOT EXISTS probe_streams (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    committed_seq BIGINT NOT NULL CHECK (committed_seq >= 0),
    retired_at DATETIME(6) NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, stream_id),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS probe_observations (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    assignment_generation BIGINT NOT NULL CHECK (assignment_generation >= 1),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    seq BIGINT NOT NULL CHECK (seq >= 1),
    config_revision BIGINT NOT NULL CHECK (config_revision >= 0),
    status TINYINT NOT NULL,
    raw_status TINYINT NOT NULL,
    down_count INT NOT NULL,
    ping INT NOT NULL DEFAULT 0,
    duration_ms INT NOT NULL DEFAULT 0,
    message TEXT NOT NULL,
    important BOOLEAN NOT NULL DEFAULT FALSE,
    observed_at DATETIME(6) NOT NULL,
    received_at DATETIME(6) NOT NULL,
    UNIQUE KEY uq_probe_obs_stream_seq (stream_id, seq),
    INDEX idx_probe_obs_monitor_probe_time (monitor_id, probe_id, observed_at, id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS monitor_probe_state (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    assignment_generation BIGINT NOT NULL CHECK (assignment_generation >= 1),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    seq BIGINT NOT NULL CHECK (seq >= 1),
    config_revision BIGINT NOT NULL CHECK (config_revision >= 0),
    status TINYINT NOT NULL,
    down_count INT NOT NULL,
    observed_at DATETIME(6) NOT NULL,
    received_at DATETIME(6) NOT NULL,
    last_success_at DATETIME(6) NULL,
    PRIMARY KEY (monitor_id, probe_id),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS probe_commands (
    command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    assignment_generation BIGINT NULL,
    expires_at DATETIME(6) NOT NULL,
    status VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    remote_confirmed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS probe_dirty_buckets (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    resolution VARCHAR(8) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    bucket DATETIME(6) NOT NULL,
    PRIMARY KEY (monitor_id, probe_id, resolution, bucket),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
