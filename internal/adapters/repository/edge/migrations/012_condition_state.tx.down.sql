-- Evaluated promotion state is source evidence, and a downgrade can neither
-- explain nor reproduce the promoted transitions already sent to the hub, so
-- populated state or retained transition events refuse to downgrade. The guard
-- deliberately runs before any rebuild so it reports itself rather than an
-- unrelated constraint failure.
CREATE TABLE edge_condition_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_condition_downgrade_guard SELECT 0 FROM edge_condition_state LIMIT 1;
INSERT INTO edge_condition_downgrade_guard SELECT 0 FROM edge_telemetry_outbox WHERE kind = 'condition.transition' LIMIT 1;
DROP TABLE edge_condition_downgrade_guard;

-- Restore the pre-capacity durable event kind set. SQLite cannot alter a CHECK,
-- so the outbox is rebuilt and every row is copied under the narrower check.
CREATE TABLE edge_telemetry_outbox_replacement (
    seq INTEGER PRIMARY KEY CHECK (typeof(seq) = 'integer' AND seq > 0),
    kind TEXT NOT NULL CHECK (kind IN ('observation', 'alert.transition', 'delivery.result', 'watchdog.transition')),
    observed_at INTEGER NOT NULL,
    payload BLOB NOT NULL CHECK (length(payload) BETWEEN 1 AND 65536)
);
INSERT INTO edge_telemetry_outbox_replacement (seq, kind, observed_at, payload) SELECT seq, kind, observed_at, payload FROM edge_telemetry_outbox;
DROP TABLE edge_telemetry_outbox;
ALTER TABLE edge_telemetry_outbox_replacement RENAME TO edge_telemetry_outbox;
CREATE INDEX idx_edge_telemetry_age ON edge_telemetry_outbox (observed_at, seq);

DROP TABLE edge_condition_state;
