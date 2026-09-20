-- Operational rollback retains the directory. Refuse to discard any identity,
-- credential or accepted configuration, including an unenrolled stream identity.
CREATE TABLE edge_identity_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO edge_identity_downgrade_guard SELECT 0 FROM edge_identity LIMIT 1;
INSERT INTO edge_identity_downgrade_guard SELECT 0 FROM edge_credentials LIMIT 1;
INSERT INTO edge_identity_downgrade_guard SELECT 0 FROM edge_config LIMIT 1;
DROP TABLE edge_identity_downgrade_guard;
DROP TABLE edge_assignments;
DROP TABLE edge_config;
DROP TABLE edge_credentials;
DROP TABLE edge_identity;
