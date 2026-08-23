-- Insights leading-transition lookup: newest important heartbeat strictly
-- before the window. Without important in the key, LIMIT 1 still walks every
-- non-important row on idx_hb_monitor_time (a stable 60s monitor is one
-- important row plus days of UP beats).
CREATE INDEX idx_hb_monitor_important_time ON heartbeats (monitor_id, important, time DESC, id DESC);
