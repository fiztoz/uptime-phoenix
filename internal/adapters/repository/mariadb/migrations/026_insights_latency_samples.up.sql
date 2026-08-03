-- Preserve the number of non-zero latency observations behind avg_ping.
-- Total checks is not a valid weight because push/maintenance/checker results
-- may have Ping=0. This is required for correct higher-level rollups and the
-- Reliability page's latency sample count.
ALTER TABLE heartbeat_1m ADD COLUMN ping_count INT NOT NULL DEFAULT 0 AFTER max_ping;
ALTER TABLE heartbeat_1h ADD COLUMN ping_count INT NOT NULL DEFAULT 0 AFTER max_ping;
ALTER TABLE heartbeat_1d ADD COLUMN ping_count INT NOT NULL DEFAULT 0 AFTER max_ping;
