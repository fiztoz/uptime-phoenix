-- Per-link toggle for including the monitor target in alerts (see MariaDB
-- 034 for rationale). Default 1: operators opt OUT per link.
ALTER TABLE monitor_notification ADD COLUMN include_target INTEGER NOT NULL DEFAULT 1;