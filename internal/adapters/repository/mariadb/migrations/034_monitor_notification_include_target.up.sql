-- Per-link toggle for including the monitor target (URL, host:port,
-- connection string, ...) in alerts. The flag lives on the monitor↔notification
-- link, not the notification, so two monitors sharing one channel can differ.
-- Default TRUE: operators opt OUT per link.
ALTER TABLE monitor_notification
    ADD COLUMN include_target BOOLEAN NOT NULL DEFAULT TRUE
        AFTER notification_id;