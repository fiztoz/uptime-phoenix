-- Active configuration pointer and applied receipt history.
CREATE TABLE probe_active_configs (
    probe_id TEXT PRIMARY KEY,
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    applied_at TIMESTAMP NOT NULL,
    assignment_count INTEGER NOT NULL CHECK (assignment_count >= 0),
    FOREIGN KEY (probe_id, revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT,
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);

CREATE TABLE probe_config_applied_receipts (
    probe_id TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    applied_at TIMESTAMP NOT NULL,
    assignment_count INTEGER NOT NULL CHECK (assignment_count >= 0),
    created_at TIMESTAMP NOT NULL,
    PRIMARY KEY (probe_id, revision),
    FOREIGN KEY (probe_id, revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE RESTRICT
);
