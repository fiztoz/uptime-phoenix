ALTER TABLE edge_regional_state ADD COLUMN current_observation BLOB NOT NULL DEFAULT X'' CHECK (length(current_observation) <= 65536);
UPDATE edge_regional_state SET current_observation = COALESCE((SELECT payload FROM edge_telemetry_outbox WHERE seq = edge_regional_state.seq AND kind = 'observation'), X'');
