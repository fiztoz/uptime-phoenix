-- Stop all writers. MariaDB DDL auto-commits. Legacy evidence is local generation one.
ALTER TABLE monitor_conditions
    ADD COLUMN probe_id VARCHAR(36) NOT NULL DEFAULT 'local' CHECK (CHAR_LENGTH(probe_id) > 0),
    ADD COLUMN assignment_generation BIGINT NOT NULL DEFAULT 1 CHECK (assignment_generation > 0),
    DROP PRIMARY KEY,
    ADD PRIMARY KEY (monitor_id, probe_id, assignment_generation, kind);
ALTER TABLE tls_info
    ADD COLUMN probe_id VARCHAR(36) NOT NULL DEFAULT 'local' CHECK (CHAR_LENGTH(probe_id) > 0),
    ADD COLUMN assignment_generation BIGINT NOT NULL DEFAULT 1 CHECK (assignment_generation > 0),
    ADD UNIQUE KEY uq_tls_info_assignment (monitor_id, probe_id, assignment_generation);
-- Install the new monitor-leading key before dropping the FK-supporting old key.
ALTER TABLE tls_info DROP INDEX monitor_id;
