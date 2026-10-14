-- Immutable pending request rounds only; no application or send authority.
CREATE TABLE probe_delivery_preparation_rounds (
    monitor_id BIGINT NOT NULL CHECK (monitor_id > 0),
    request_round BIGINT NOT NULL CHECK (request_round > 0),
    round_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL UNIQUE CHECK (length(round_id) = 36),
    assignment_revision BIGINT NOT NULL CHECK (assignment_revision > 0),
    hub_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (length(hub_id) = 36),
    alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (alert_delivery IN ('aggregate', 'both')),
    created_at DATETIME(6) NOT NULL,
    expires_at DATETIME(6) NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (monitor_id, request_round),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
) ENGINE=InnoDB;
CREATE TABLE probe_delivery_preparation_members (
    monitor_id BIGINT NOT NULL,
    request_round BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT NOT NULL CHECK (generation > 0),
    PRIMARY KEY (monitor_id, request_round, probe_id),
    UNIQUE (monitor_id, request_round, probe_id, generation),
    FOREIGN KEY (monitor_id, request_round) REFERENCES probe_delivery_preparation_rounds(monitor_id, request_round) ON DELETE CASCADE
) ENGINE=InnoDB;
CREATE TABLE probe_delivery_preparation_requests (
    request_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (length(request_id) = 36),
    monitor_id BIGINT NOT NULL,
    request_round BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    generation BIGINT NOT NULL,
    config_revision BIGINT NOT NULL CHECK (config_revision > 0),
    stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (length(stream_id) = 36),
    sha256 VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL CHECK (sha256 REGEXP '^[0-9a-f]{64}$'),
    command_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    prepared_at DATETIME(6) NOT NULL,
    PRIMARY KEY (request_id),
    UNIQUE (monitor_id, request_round, probe_id, config_revision, stream_id),
    CHECK ((probe_id = 'local' AND stream_id = '00000000-0000-4000-8000-000000000001' AND command_id IS NULL) OR (probe_id <> 'local' AND stream_id <> '00000000-0000-4000-8000-000000000001' AND command_id IS NOT NULL AND command_id = request_id)),
    FOREIGN KEY (monitor_id, request_round, probe_id, generation) REFERENCES probe_delivery_preparation_members(monitor_id, request_round, probe_id, generation) ON DELETE CASCADE,
    FOREIGN KEY (probe_id, config_revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (command_id) REFERENCES probe_commands(command_id) ON DELETE RESTRICT
) ENGINE=InnoDB;
