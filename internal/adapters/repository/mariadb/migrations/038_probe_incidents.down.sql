-- Run only with all application processes stopped; MariaDB DDL auto-commits.
-- A failed guard is intentional: scoped incidents would be discarded.

CREATE TABLE IF NOT EXISTS probe_incident_down_guard (
    state_count BIGINT NOT NULL CHECK (state_count = 0)
) ENGINE=InnoDB;
DELETE FROM probe_incident_down_guard;
INSERT INTO probe_incident_down_guard (state_count)
SELECT (SELECT COUNT(*) FROM probe_incidents)
     + (SELECT COUNT(*) FROM probe_delivery_events);
DROP TABLE probe_delivery_events;
DROP TABLE probe_incidents;
DROP TABLE probe_incident_down_guard;
