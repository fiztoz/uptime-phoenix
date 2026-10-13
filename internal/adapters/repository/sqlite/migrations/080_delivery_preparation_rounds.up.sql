-- Immutable pending request rounds only; no application or send authority.
CREATE TABLE probe_delivery_preparation_rounds (
    monitor_id INTEGER NOT NULL CHECK (typeof(monitor_id) = 'integer' AND monitor_id > 0),
    request_round INTEGER NOT NULL CHECK (typeof(request_round) = 'integer' AND request_round > 0),
    round_id TEXT NOT NULL UNIQUE CHECK (length(round_id) = 36),
    assignment_revision INTEGER NOT NULL CHECK (typeof(assignment_revision) = 'integer' AND assignment_revision > 0),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate', 'both')),
    created_at TIMESTAMP NOT NULL,
    expires_at TIMESTAMP NOT NULL CHECK (expires_at > created_at),
    PRIMARY KEY (monitor_id, request_round),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
CREATE TABLE probe_delivery_preparation_members (
    monitor_id INTEGER NOT NULL,
    request_round INTEGER NOT NULL,
    probe_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
    PRIMARY KEY (monitor_id, request_round, probe_id),
    UNIQUE (monitor_id, request_round, probe_id, generation),
    FOREIGN KEY (monitor_id, request_round) REFERENCES probe_delivery_preparation_rounds(monitor_id, request_round) ON DELETE CASCADE
);
CREATE TABLE probe_delivery_preparation_requests (
    request_id TEXT NOT NULL CHECK (length(request_id) = 36),
    monitor_id INTEGER NOT NULL,
    request_round INTEGER NOT NULL,
    probe_id TEXT NOT NULL,
    generation INTEGER NOT NULL,
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision > 0),
    stream_id TEXT NOT NULL CHECK (length(stream_id) = 36),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    command_id TEXT NULL,
    prepared_at TIMESTAMP NOT NULL,
    PRIMARY KEY (request_id),
    UNIQUE (monitor_id, request_round, probe_id, config_revision, stream_id),
    CHECK ((probe_id = 'local' AND stream_id = '00000000-0000-4000-8000-000000000001' AND command_id IS NULL) OR (probe_id <> 'local' AND stream_id <> '00000000-0000-4000-8000-000000000001' AND command_id IS NOT NULL AND command_id = request_id)),
    FOREIGN KEY (monitor_id, request_round, probe_id, generation) REFERENCES probe_delivery_preparation_members(monitor_id, request_round, probe_id, generation) ON DELETE CASCADE,
    FOREIGN KEY (probe_id, config_revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (command_id) REFERENCES probe_commands(command_id) ON DELETE RESTRICT
);
