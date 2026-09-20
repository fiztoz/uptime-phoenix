-- Stop runtime/config writers before rollback. Export operator intent first.
DROP TABLE IF EXISTS probe_watchdog_notifications;
DROP TABLE IF EXISTS probe_watchdog_settings;
