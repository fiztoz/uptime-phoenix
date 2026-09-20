CREATE TEMP TABLE edge_retention_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO edge_retention_downgrade_guard SELECT COUNT(*) FROM edge_gaps;
DROP TABLE edge_retention_downgrade_guard;
DROP INDEX idx_edge_telemetry_age;
DROP TABLE edge_gaps;
