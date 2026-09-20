CREATE TABLE IF NOT EXISTS probe_runtime_owners (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (probe_id <> 'local'),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    epoch BIGINT NOT NULL CHECK (epoch >= 1),
    lease_until BIGINT NOT NULL CHECK (lease_until >= 0),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
