-- Stop all writers. MariaDB DDL auto-commits. Existing rows remain local generation one.
ALTER TABLE alerts
    ADD COLUMN probe_id VARCHAR(36) NOT NULL DEFAULT 'local' CHECK (CHAR_LENGTH(probe_id) > 0),
    ADD COLUMN assignment_generation BIGINT NOT NULL DEFAULT 1 CHECK (assignment_generation > 0),
    ADD UNIQUE KEY uq_alerts_open_assignment (open_monitor_id, probe_id, assignment_generation),
    DROP INDEX uq_alerts_open_monitor;
-- Escalation ownership is inherited via the unchanged alert_id FK and UNIQUE key.
