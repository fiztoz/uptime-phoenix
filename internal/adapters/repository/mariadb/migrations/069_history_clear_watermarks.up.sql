-- Clear-history watermarks fence deliberate deletion against replay. One row
-- per (monitor, probe, assignment generation) records the explicit bound the
-- operator cleared through and how many later replayed observations were
-- deliberately dropped under it. A repeated clear raises both bounds (GREATEST)
-- so a backward hub clock can never shrink the fence. The row is the durable
-- acknowledgement of an intentional drop; ingest refuses to resurrect cleared
-- evidence and counts every refusal here.
CREATE TABLE IF NOT EXISTS history_clear_watermarks (
    monitor_id BIGINT NOT NULL,
    probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    assignment_generation BIGINT NOT NULL CHECK (assignment_generation >= 1),
    clear_id CHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    through_seq BIGINT NOT NULL CHECK (through_seq >= 0),
    through_observed_at DATETIME(6) NOT NULL,
    cleared_at DATETIME(6) NOT NULL,
    dropped_count BIGINT NOT NULL DEFAULT 0 CHECK (dropped_count >= 0),
    PRIMARY KEY (monitor_id, probe_id, assignment_generation),
    CONSTRAINT fk_history_clear_monitor FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    CONSTRAINT fk_history_clear_probe FOREIGN KEY (probe_id) REFERENCES probes(id) ON DELETE RESTRICT
) ENGINE=InnoDB;
