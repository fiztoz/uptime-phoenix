-- Stop all writers. MariaDB DDL auto-commits. Preserve existing IDs, tokens and children.
ALTER TABLE alerts
    ADD COLUMN source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN transition_version BIGINT NOT NULL DEFAULT 1 CHECK (transition_version > 0);
-- Explicit self-assignment suppresses the existing ON UPDATE timestamp behavior.
UPDATE alerts SET source_alert_id = LOWER(UUID()), updated_at = updated_at WHERE source_alert_id IS NULL;
ALTER TABLE alerts
    MODIFY COLUMN source_alert_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ADD CONSTRAINT ck_alerts_source_identity CHECK (source_alert_id REGEXP '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' AND source_alert_id <> '00000000-0000-0000-0000-000000000000'),
    ADD UNIQUE KEY uq_alerts_source_identity (source_alert_id);
