-- Saved source intent; default absence means disabled. No runtime is armed here.
CREATE TABLE IF NOT EXISTS probe_watchdog_settings (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL PRIMARY KEY CHECK (probe_id <> 'local'),
    revision BIGINT NOT NULL CHECK (revision > 0),
    enabled BOOLEAN NOT NULL CHECK (enabled IN (0,1)),
    lost_after_seconds BIGINT NOT NULL CHECK (lost_after_seconds BETWEEN 1 AND 2147483647),
    recover_after_seconds BIGINT NOT NULL CHECK (recover_after_seconds BETWEEN 1 AND 2147483647),
    resend_interval BIGINT NOT NULL CHECK (resend_interval BETWEEN 0 AND 153722867),
    FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
CREATE TABLE IF NOT EXISTS probe_watchdog_notifications (
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    notification_id BIGINT NOT NULL,
    PRIMARY KEY (probe_id, notification_id),
    FOREIGN KEY (probe_id) REFERENCES probe_watchdog_settings(probe_id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE
) ENGINE=InnoDB;
