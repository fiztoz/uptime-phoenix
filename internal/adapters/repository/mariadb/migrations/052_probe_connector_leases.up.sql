CREATE TABLE IF NOT EXISTS probe_sessions (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY CHECK (probe_id <> 'local'),
    owner_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT NOT NULL CHECK (generation >= 1),
    lease_until BIGINT NOT NULL CHECK (lease_until >= 0),
    connected BOOLEAN NOT NULL CHECK (connected IN (0, 1)),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
