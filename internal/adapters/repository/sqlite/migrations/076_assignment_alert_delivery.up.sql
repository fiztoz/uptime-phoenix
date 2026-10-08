-- Desired alert delivery lives with the assignment set and its effective history.
-- Existing rows are regional. Storing aggregate or both does not activate hub
-- paging, suppress regional delivery, or prove a source applied the mode.
ALTER TABLE monitor_probe_assignment_sets
    ADD COLUMN alert_delivery TEXT NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional', 'aggregate', 'both'));
ALTER TABLE monitor_probe_assignment_history
    ADD COLUMN alert_delivery TEXT NOT NULL DEFAULT 'regional' CHECK (alert_delivery IN ('regional', 'aggregate', 'both'));
