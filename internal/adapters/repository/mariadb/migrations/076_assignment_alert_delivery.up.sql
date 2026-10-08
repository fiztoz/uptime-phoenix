-- Desired alert delivery lives with the assignment set and its effective history.
-- Existing rows are regional. Storing aggregate or both does not activate hub
-- paging, suppress regional delivery, or prove a source applied the mode.
ALTER TABLE monitor_probe_assignment_sets
    ADD COLUMN IF NOT EXISTS alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional', 'aggregate', 'both'));
ALTER TABLE monitor_probe_assignment_history
    ADD COLUMN IF NOT EXISTS alert_delivery VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional', 'aggregate', 'both'));
