-- Stop all writers. Refuse to discard persisted backoff for an outstanding attempt.
CREATE TABLE IF NOT EXISTS notification_throttles_down_guard (
    state_count INTEGER NOT NULL CHECK (state_count = 0)
);
DELETE FROM notification_throttles_down_guard;
INSERT INTO notification_throttles_down_guard (state_count)
SELECT COUNT(*) FROM notification_throttles;
DROP TABLE notification_throttles;
DROP TABLE notification_throttles_down_guard;
