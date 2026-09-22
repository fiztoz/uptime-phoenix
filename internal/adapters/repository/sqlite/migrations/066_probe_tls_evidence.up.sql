-- Exact sanitized TLS evidence follows the immutable observation and current sequence.
ALTER TABLE probe_observations ADD COLUMN tls_json TEXT NULL CHECK (tls_json IS NULL OR json_valid(tls_json));
ALTER TABLE monitor_probe_state ADD COLUMN tls_json TEXT NULL CHECK (tls_json IS NULL OR json_valid(tls_json));
