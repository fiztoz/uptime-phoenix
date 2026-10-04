CREATE TABLE edge_observation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_observation_downgrade_guard SELECT 0 FROM edge_identity WHERE last_created_seq > 0 LIMIT 1;
INSERT INTO edge_observation_downgrade_guard SELECT 0 FROM edge_delivery_outbox LIMIT 1;
DROP TABLE edge_observation_downgrade_guard;
DROP TABLE edge_delivery_outbox;
DROP TABLE edge_telemetry_outbox;
DROP TABLE edge_alerts;
DROP TABLE edge_regional_state;
