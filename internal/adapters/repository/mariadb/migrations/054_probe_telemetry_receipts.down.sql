-- Receipts prove permanent outcomes for already-pruned edge data.
CREATE TABLE IF NOT EXISTS probe_replay_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_replay_downgrade_guard SELECT 0 FROM probe_telemetry_receipts LIMIT 1;
DROP TABLE probe_replay_downgrade_guard;
DROP TABLE probe_telemetry_receipts;
