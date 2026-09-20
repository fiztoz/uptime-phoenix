-- Saved source intent; default absence means disabled. No runtime is armed here.
CREATE TABLE IF NOT EXISTS probe_watchdog_settings (
    probe_id TEXT NOT NULL PRIMARY KEY CHECK (probe_id <> 'local'),
    revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0),
    enabled BOOLEAN NOT NULL CHECK (typeof(enabled) = 'integer' AND enabled IN (0,1)),
    lost_after_seconds INTEGER NOT NULL CHECK (typeof(lost_after_seconds) = 'integer' AND lost_after_seconds BETWEEN 1 AND 2147483647),
    recover_after_seconds INTEGER NOT NULL CHECK (typeof(recover_after_seconds) = 'integer' AND recover_after_seconds BETWEEN 1 AND 2147483647),
    resend_interval INTEGER NOT NULL CHECK (typeof(resend_interval) = 'integer' AND resend_interval BETWEEN 0 AND 153722867),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS probe_watchdog_notifications (
    probe_id TEXT NOT NULL,
    notification_id INTEGER NOT NULL,
    PRIMARY KEY (probe_id, notification_id),
    FOREIGN KEY (probe_id) REFERENCES probe_watchdog_settings(probe_id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
);
