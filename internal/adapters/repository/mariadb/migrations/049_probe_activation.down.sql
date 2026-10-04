-- Stop writers. Active configuration state and receipts must not be silently discarded.
CREATE TABLE IF NOT EXISTS probe_activation_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO probe_activation_downgrade_guard (ok) SELECT 0 FROM probe_active_configs LIMIT 1;
INSERT INTO probe_activation_downgrade_guard (ok) SELECT 0 FROM probe_config_applied_receipts LIMIT 1;
DROP TABLE probe_activation_downgrade_guard;
DROP TABLE IF EXISTS probe_config_applied_receipts;
DROP TABLE IF EXISTS probe_active_configs;
