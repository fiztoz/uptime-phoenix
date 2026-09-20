-- Prepared credentials can already be active remotely after a lost receipt.
-- Never discard their only recoverable plaintext source during downgrade.
CREATE TABLE IF NOT EXISTS probe_connection_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_connection_downgrade_guard SELECT 0 FROM probe_connections LIMIT 1;
DROP TABLE probe_connection_downgrade_guard;
DROP TABLE probe_connections;
