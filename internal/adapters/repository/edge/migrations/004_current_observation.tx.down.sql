CREATE TEMPORARY TABLE edge_current_state_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO edge_current_state_downgrade_guard SELECT COUNT(*) FROM edge_regional_state WHERE length(current_observation) > 0;
DROP TABLE edge_current_state_downgrade_guard;
ALTER TABLE edge_regional_state DROP COLUMN current_observation;
