-- Generation tombstones must survive config retirement. Their revision remains
-- an authority watermark; current activation validates its selected config.
CREATE TABLE edge_assignments_replacement (
 monitor_id INTEGER PRIMARY KEY CHECK (monitor_id > 0),
 generation INTEGER NOT NULL CHECK (typeof(generation) = 'integer' AND generation > 0),
 active INTEGER NOT NULL CHECK (active IN (0, 1)),
 revision INTEGER NOT NULL CHECK (typeof(revision) = 'integer' AND revision > 0)
);
INSERT INTO edge_assignments_replacement SELECT monitor_id,generation,active,revision FROM edge_assignments;
DROP TABLE edge_assignments;
ALTER TABLE edge_assignments_replacement RENAME TO edge_assignments;

-- A separate 64 MiB metadata quota includes conservative row/index overhead.
-- Existing over-budget data is preserved; deletion/non-growing updates still work.
CREATE TABLE edge_metadata_budget (
 id INTEGER PRIMARY KEY CHECK (id = 1),
 used_bytes INTEGER NOT NULL CHECK (typeof(used_bytes) = 'integer' AND used_bytes >= 0)
);
INSERT INTO edge_metadata_budget (id, used_bytes) SELECT 1,
(SELECT COALESCE(SUM(length(protected_payload) + 512),0) FROM edge_config)
 + (SELECT COALESCE(SUM(256),0) FROM edge_assignments)
 + (SELECT COALESCE(SUM(length(current_observation) + 512),0) FROM edge_regional_state)
 + (SELECT COALESCE(SUM(length(CAST(reason AS BLOB)) + length(CAST(COALESCE(ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(ack_note, '') AS BLOB)) + 1024),0) FROM edge_alerts)
 + (SELECT COALESCE(SUM(512),0) FROM edge_watchdog_state);
CREATE TRIGGER edge_config_metadata_insert AFTER INSERT ON edge_config
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(NEW.protected_payload) + 512)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_config_metadata_update AFTER UPDATE ON edge_config
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(NEW.protected_payload) + 512) - (length(OLD.protected_payload) + 512)) WHERE id = 1;
 SELECT CASE WHEN ((length(NEW.protected_payload) + 512) > (length(OLD.protected_payload) + 512)) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_config_metadata_delete AFTER DELETE ON edge_config
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (length(OLD.protected_payload) + 512)) WHERE id = 1;
END;
CREATE TRIGGER edge_assignments_metadata_insert AFTER INSERT ON edge_assignments
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((256)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_assignments_metadata_delete AFTER DELETE ON edge_assignments
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (256)) WHERE id = 1;
END;
CREATE TRIGGER edge_regional_state_metadata_insert AFTER INSERT ON edge_regional_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(NEW.current_observation) + 512)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_regional_state_metadata_update AFTER UPDATE ON edge_regional_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(NEW.current_observation) + 512) - (length(OLD.current_observation) + 512)) WHERE id = 1;
 SELECT CASE WHEN ((length(NEW.current_observation) + 512) > (length(OLD.current_observation) + 512)) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_regional_state_metadata_delete AFTER DELETE ON edge_regional_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (length(OLD.current_observation) + 512)) WHERE id = 1;
END;
CREATE TRIGGER edge_alerts_metadata_insert AFTER INSERT ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_alerts_metadata_update AFTER UPDATE ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024) - (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
 SELECT CASE WHEN ((length(CAST(NEW.reason AS BLOB)) + length(CAST(COALESCE(NEW.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(NEW.ack_note, '') AS BLOB)) + 1024) > (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_alerts_metadata_delete AFTER DELETE ON edge_alerts
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (length(CAST(OLD.reason AS BLOB)) + length(CAST(COALESCE(OLD.ack_actor_display_name, '') AS BLOB)) + length(CAST(COALESCE(OLD.ack_note, '') AS BLOB)) + 1024)) WHERE id = 1;
END;
CREATE TRIGGER edge_watchdog_state_metadata_insert AFTER INSERT ON edge_watchdog_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + ((512)) WHERE id = 1;
 SELECT CASE WHEN (1) AND (SELECT used_bytes FROM edge_metadata_budget WHERE id = 1) > 67108864 THEN RAISE(ABORT, 'edge metadata capacity exceeded') END;
END;
CREATE TRIGGER edge_watchdog_state_metadata_delete AFTER DELETE ON edge_watchdog_state
BEGIN
 UPDATE edge_metadata_budget SET used_bytes = used_bytes + (- (512)) WHERE id = 1;
END;
CREATE INDEX idx_edge_config_retirement ON edge_config (applied_at, revision);
CREATE INDEX idx_edge_state_retirement ON edge_regional_state (observed_at, monitor_id, generation);
CREATE INDEX idx_edge_alert_retirement ON edge_alerts (status, resolved_at, source_alert_id);
CREATE INDEX idx_edge_delivery_retirement ON edge_delivery_outbox (status, outcome_at, delivery_id);
CREATE INDEX idx_edge_state_config_ref ON edge_regional_state (config_revision);
CREATE INDEX idx_edge_state_alert_ref ON edge_regional_state (source_alert_id);
CREATE INDEX idx_edge_alert_config_ref ON edge_alerts (config_revision);
CREATE INDEX idx_edge_alert_generation_ref ON edge_alerts (monitor_id,generation,status);
CREATE INDEX idx_edge_delivery_config_ref ON edge_delivery_outbox (config_revision);
CREATE INDEX idx_edge_delivery_alert_ref ON edge_delivery_outbox (source_alert_id);
