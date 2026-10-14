-- Source-local effective mode only; desired modes never authorize suppression.
-- Legacy rows are not backfilled with fabricated application evidence.
CREATE TABLE edge_regional_delivery_applications (
    config_revision INTEGER NOT NULL CHECK (typeof(config_revision) = 'integer' AND config_revision > 0),
    monitor_id INTEGER NOT NULL CHECK (typeof(monitor_id) = 'integer' AND monitor_id > 0),
    stream_id TEXT NOT NULL CHECK (length(stream_id) = 36),
    assignment_revision INTEGER NOT NULL CHECK (typeof(assignment_revision) = 'integer' AND assignment_revision > 0),
    generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
    alert_delivery TEXT NOT NULL CHECK (alert_delivery = 'regional'),
    PRIMARY KEY (config_revision, monitor_id, stream_id),
    FOREIGN KEY (config_revision) REFERENCES edge_config(revision) ON DELETE CASCADE
);
