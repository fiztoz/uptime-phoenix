-- Never silently discard retained idempotency receipts on downgrade.
CREATE TABLE edge_commands_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_commands_downgrade_guard SELECT 0 FROM edge_applied_commands LIMIT 1;
DROP TABLE edge_commands_downgrade_guard;
DROP TABLE edge_applied_commands;
