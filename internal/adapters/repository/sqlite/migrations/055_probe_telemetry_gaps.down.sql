CREATE TEMPORARY TABLE probe_gap_downgrade_guard (n INTEGER CHECK (n = 0));
INSERT INTO probe_gap_downgrade_guard SELECT COUNT(*) FROM probe_telemetry_gaps;
DROP TABLE probe_gap_downgrade_guard;
DROP TABLE probe_telemetry_gaps;
