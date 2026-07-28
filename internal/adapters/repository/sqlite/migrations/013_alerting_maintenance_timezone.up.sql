-- Sprint C Track A: certificate-expiry opt-in + maintenance cron timezone.
-- TLS alert threshold state is stored inside tls_info.info_json (no column).

ALTER TABLE monitors ADD COLUMN cert_expiry_notify INTEGER NOT NULL DEFAULT 0;

ALTER TABLE maintenance_windows ADD COLUMN timezone TEXT NOT NULL DEFAULT 'UTC';
