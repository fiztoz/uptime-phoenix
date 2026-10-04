-- No request payload is stored: credentials and operator notes must not be
-- duplicated in the command ledger. Identity binds exact bytes and typed target.
CREATE TABLE edge_applied_commands (
    command_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL,
    request_hash BLOB NOT NULL CHECK (length(request_hash) = 32),
    status TEXT NOT NULL CHECK (status IN ('applied', 'already_applied', 'already_resolved', 'rejected', 'expired')),
    applied_at INTEGER,
    code TEXT NOT NULL,
    message TEXT NOT NULL,
    retain_until INTEGER NOT NULL,
    CHECK ((status IN ('applied', 'already_applied', 'already_resolved') AND applied_at IS NOT NULL AND code = '') OR
           (status = 'rejected' AND applied_at IS NULL AND code <> '') OR
           (status = 'expired' AND applied_at IS NULL))
);
CREATE INDEX idx_edge_command_retention ON edge_applied_commands (retain_until, command_id);
