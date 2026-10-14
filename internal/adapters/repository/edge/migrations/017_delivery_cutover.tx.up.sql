-- Operative aggregate/both cutover storage: kind-specific success receipts,
-- monitor-wide source gates and final-authorization provider-attempt grants.
-- The receipt ledger keeps every populated column; only its success-code
-- constraint grows for the three new delivery cutover kinds.
CREATE TEMP TABLE edge_delivery_preparations_hold AS SELECT * FROM edge_delivery_preparations;
DROP TABLE edge_delivery_preparations;
CREATE TABLE edge_applied_commands_cutover (
 command_id TEXT PRIMARY KEY,
 kind TEXT NOT NULL,
 request_hash BLOB NOT NULL CHECK (length(request_hash) = 32),
 status TEXT NOT NULL CHECK (status IN ('applied', 'already_applied', 'already_resolved', 'rejected', 'expired')),
 applied_at INTEGER,
 code TEXT NOT NULL,
 message TEXT NOT NULL,
 retain_until INTEGER NOT NULL,
 credential_version INTEGER CHECK (credential_version IS NULL OR (typeof(credential_version) = 'integer' AND credential_version > 0 AND kind = 'credential.prepare' AND status IN ('applied', 'already_applied'))),
 certificate_version INTEGER CHECK (certificate_version IS NULL OR (typeof(certificate_version) = 'integer' AND certificate_version > 1 AND kind = 'certificate.prepare' AND status IN ('applied', 'already_applied'))),
 certificate_fingerprint TEXT CHECK (certificate_fingerprint IS NULL OR (length(certificate_fingerprint) = 64 AND certificate_version IS NOT NULL)),
 certificate_not_after INTEGER CHECK (certificate_not_after IS NULL OR (typeof(certificate_not_after) = 'integer' AND certificate_version IS NOT NULL)),
 CHECK ((kind NOT IN ('delivery.prepare', 'delivery.quiesce', 'delivery.apply', 'delivery.resume') AND status IN ('applied', 'already_applied', 'already_resolved') AND applied_at IS NOT NULL AND code = '') OR
        (kind = 'delivery.prepare' AND status IN ('applied', 'already_applied') AND applied_at IS NOT NULL AND code = 'delivery_prepared') OR
        (kind = 'delivery.quiesce' AND status IN ('applied', 'already_applied') AND applied_at IS NOT NULL AND code = 'delivery_quiesced') OR
        (kind = 'delivery.apply' AND status IN ('applied', 'already_applied') AND applied_at IS NOT NULL AND code = 'delivery_applied') OR
        (kind = 'delivery.resume' AND status IN ('applied', 'already_applied') AND applied_at IS NOT NULL AND code = 'delivery_resumed') OR
        (status = 'rejected' AND applied_at IS NULL AND code <> '') OR
        (status = 'expired' AND applied_at IS NULL))
);
INSERT INTO edge_applied_commands_cutover SELECT * FROM edge_applied_commands;
DROP TABLE edge_applied_commands;
ALTER TABLE edge_applied_commands_cutover RENAME TO edge_applied_commands;
CREATE INDEX idx_edge_command_retention ON edge_applied_commands (retain_until, command_id);
CREATE TABLE edge_delivery_preparations (
 command_id TEXT PRIMARY KEY REFERENCES edge_applied_commands(command_id) ON DELETE CASCADE DEFERRABLE INITIALLY DEFERRED,
 hub_id TEXT NOT NULL,
 probe_id TEXT NOT NULL,
 stream_id TEXT NOT NULL,
 monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
 assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
 generation INTEGER NOT NULL CHECK (generation > 0),
 config_revision INTEGER NOT NULL REFERENCES edge_config(revision) ON DELETE RESTRICT CHECK (config_revision > 0),
 sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
 alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate', 'both')),
 prepared_at INTEGER NOT NULL CHECK (prepared_at > 0)
);
INSERT INTO edge_delivery_preparations SELECT * FROM edge_delivery_preparations_hold;
DROP TABLE edge_delivery_preparations_hold;
-- Monitor-wide source gates. One row per immutable barrier; only delivery.resume
-- releases a gate, so configuration activation, session changes, stream reset and
-- time can never erase a hold. A partial unique index keeps at most one live gate
-- per monitor, so a new barrier cannot bypass a drain in progress.
CREATE TABLE edge_delivery_cutover_gates (
 barrier_id TEXT PRIMARY KEY CHECK (length(barrier_id) = 36),
 monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
 assignment_revision INTEGER NOT NULL CHECK (assignment_revision > 0),
 generation INTEGER NOT NULL CHECK (generation > 0),
 config_revision INTEGER NOT NULL CHECK (config_revision > 0),
 sha256 TEXT NOT NULL CHECK (length(sha256) = 64 AND sha256 NOT GLOB '*[^0-9a-f]*'),
 stream_id TEXT NOT NULL CHECK (length(stream_id) = 36),
 alert_delivery TEXT NOT NULL CHECK (alert_delivery IN ('aggregate', 'both')),
 preparation_command_id TEXT NOT NULL CHECK (length(preparation_command_id) = 36),
 state TEXT NOT NULL CHECK (state IN ('quiescing', 'quiesced', 'applied', 'released')),
 established_at INTEGER NOT NULL CHECK (established_at > 0),
 quiesced_at INTEGER,
 applied_at INTEGER,
 released_at INTEGER,
 CHECK ((state = 'quiescing' AND quiesced_at IS NULL AND applied_at IS NULL AND released_at IS NULL) OR
        (state = 'quiesced' AND quiesced_at IS NOT NULL AND applied_at IS NULL AND released_at IS NULL) OR
        (state = 'applied' AND quiesced_at IS NOT NULL AND applied_at IS NOT NULL AND released_at IS NULL) OR
        (state = 'released' AND released_at IS NOT NULL))
);
CREATE UNIQUE INDEX idx_edge_cutover_gate_live_monitor ON edge_delivery_cutover_gates (monitor_id) WHERE state <> 'released';
-- Provider-attempt journal for final availability authorization. Grants record
-- I/O facts only: unknown, crashed or errored attempts stay unresolved and block
-- quiescence/drain. They are never cleared by timers, lease expiry or restart.
CREATE TABLE edge_provider_attempts (
 attempt_id TEXT PRIMARY KEY CHECK (length(attempt_id) >= 1 AND length(attempt_id) <= 128),
 delivery_id TEXT NOT NULL CHECK (length(delivery_id) = 36),
 attempt INTEGER NOT NULL CHECK (attempt > 0),
 monitor_id INTEGER NOT NULL CHECK (monitor_id > 0),
 state TEXT NOT NULL CHECK (state IN ('in_flight', 'succeeded', 'ambiguous', 'not_started')),
 granted_at INTEGER NOT NULL CHECK (granted_at > 0),
 finished_at INTEGER,
 CHECK ((state = 'in_flight' AND finished_at IS NULL) OR (state <> 'in_flight' AND finished_at IS NOT NULL))
);
CREATE UNIQUE INDEX idx_edge_provider_attempt_claim ON edge_provider_attempts (delivery_id, attempt);
CREATE INDEX idx_edge_provider_attempt_monitor ON edge_provider_attempts (monitor_id, state);
-- Known attempt states are immutable; only an in_flight grant may be finished.
CREATE TRIGGER edge_provider_attempt_state_immutable
BEFORE UPDATE OF state ON edge_provider_attempts
WHEN OLD.state <> 'in_flight' AND NEW.state <> OLD.state
BEGIN SELECT RAISE(ABORT, 'provider attempt state is immutable'); END;
-- Conservatively retain legacy availability work whose provider I/O completion
-- cannot be proven: unfinished leased work and attempts that ended in an error.
-- Recorded successes and recorded superseded outcomes are not reinterpreted and
-- no success is fabricated back into the journal.
INSERT INTO edge_provider_attempts (attempt_id, delivery_id, attempt, monitor_id, state, granted_at, finished_at)
SELECT 'legacy-' || delivery_id || '-' || attempt, delivery_id, attempt, monitor_id, 'ambiguous',
       COALESCE(leased_at, created_at), CAST(strftime('%s', 'now') AS INTEGER) * 1000000
FROM edge_delivery_outbox
WHERE monitor_id > 0 AND attempt > 0 AND event_kind IN ('status_change', 'incident_summary')
  AND status NOT IN ('sent', 'superseded');
