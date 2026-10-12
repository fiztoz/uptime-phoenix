-- Source-local effective regional state only; no desired-mode backfill.
CREATE TABLE probe_local_regional_applications (
    probe_id TEXT NOT NULL CHECK (probe_id = 'local'),
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision > 0),
    monitor_id INTEGER NOT NULL CHECK (typeof(monitor_id) = 'integer' AND monitor_id > 0),
    assignment_revision INTEGER NOT NULL CHECK (typeof(assignment_revision) = 'integer' AND assignment_revision > 0),
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
    hub_id TEXT NOT NULL CHECK (length(hub_id) = 36),
    stream_id TEXT NOT NULL CHECK (stream_id = '00000000-0000-4000-8000-000000000001'),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery = 'regional'),
    PRIMARY KEY (probe_id, config_revision, monitor_id),
    FOREIGN KEY (probe_id, config_revision) REFERENCES probe_config_snapshots(probe_id, revision) ON DELETE CASCADE
);
