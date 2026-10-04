-- Active configuration pointer and applied receipt history.
CREATE TABLE probe_active_configs (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,
    revision BIGINT NOT NULL CHECK (revision > 0),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    applied_at DATETIME(6) NOT NULL,
    assignment_count INT NOT NULL CHECK (assignment_count >= 0),
    FOREIGN KEY (probe_id, revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;

CREATE TABLE probe_config_applied_receipts (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    revision BIGINT NOT NULL CHECK (revision > 0),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (CHAR_LENGTH(hub_id) = 36),
    applied_at DATETIME(6) NOT NULL,
    assignment_count INT NOT NULL CHECK (assignment_count >= 0),
    created_at DATETIME(6) NOT NULL,
    PRIMARY KEY (probe_id, revision),
    FOREIGN KEY (probe_id, revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT
) ENGINE=InnoDB;
