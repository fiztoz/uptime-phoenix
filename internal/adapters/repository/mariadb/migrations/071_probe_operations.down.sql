ALTER TABLE probes DROP COLUMN tls_fingerprint;
ALTER TABLE probes DROP COLUMN endpoint;
DROP TABLE IF EXISTS probe_operations;
