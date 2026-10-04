-- Expiry stays in JSON to preserve protocol precision independently of DATETIME(6).
ALTER TABLE probe_observations ADD COLUMN tls_json JSON NULL;
ALTER TABLE monitor_probe_state ADD COLUMN tls_json JSON NULL;
