CREATE TEMPORARY TABLE edge_metadata_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO edge_metadata_downgrade_guard
 SELECT COUNT(*) FROM edge_assignments a WHERE NOT EXISTS (SELECT 1 FROM edge_config c WHERE c.revision = a.revision);
DROP TABLE edge_metadata_downgrade_guard;
DROP TRIGGER edge_config_metadata_insert;
DROP TRIGGER edge_config_metadata_update;
DROP TRIGGER edge_config_metadata_delete;
DROP TRIGGER edge_assignments_metadata_insert;
DROP TRIGGER edge_assignments_metadata_delete;
DROP TRIGGER edge_regional_state_metadata_insert;
DROP TRIGGER edge_regional_state_metadata_update;
DROP TRIGGER edge_regional_state_metadata_delete;
DROP TRIGGER edge_alerts_metadata_insert;
DROP TRIGGER edge_alerts_metadata_update;
DROP TRIGGER edge_alerts_metadata_delete;
DROP TRIGGER edge_watchdog_state_metadata_insert;
DROP TRIGGER edge_watchdog_state_metadata_delete;
DROP TABLE edge_metadata_budget;
DROP INDEX idx_edge_state_config_ref;
DROP INDEX idx_edge_state_alert_ref;
DROP INDEX idx_edge_alert_config_ref;
DROP INDEX idx_edge_alert_generation_ref;
DROP INDEX idx_edge_delivery_config_ref;
DROP INDEX idx_edge_delivery_alert_ref;

DROP INDEX idx_edge_config_retirement;
DROP INDEX idx_edge_state_retirement;
DROP INDEX idx_edge_alert_retirement;
DROP INDEX idx_edge_delivery_retirement;
CREATE TABLE edge_assignments_replacement (
 monitor_id INTEGER PRIMARY KEY CHECK (monitor_id > 0),
 generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
 active INTEGER NOT NULL CHECK (active IN (0, 1)),
 revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0) REFERENCES edge_config(revision)
);
INSERT INTO edge_assignments_replacement SELECT monitor_id,generation,active,revision FROM edge_assignments;
DROP TABLE edge_assignments;
ALTER TABLE edge_assignments_replacement RENAME TO edge_assignments;
