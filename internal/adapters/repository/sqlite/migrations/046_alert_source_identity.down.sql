-- Unpublished mappings may be regenerated after downgrade. Referenced identities must survive.
CREATE TABLE IF NOT EXISTS alert_source_downgrade_guard (ok INTEGER NOT NULL CHECK (ok = 1));
INSERT INTO alert_source_downgrade_guard (ok)
SELECT 0 FROM alerts JOIN probe_incidents ON probe_incidents.source_alert_id = alerts.source_alert_id LIMIT 1;
DROP TABLE alert_source_downgrade_guard;
-- Stop all writers. Execute this entire SQLite migration in one transaction.
CREATE TABLE alert_source_sequences AS SELECT name, seq FROM sqlite_sequence WHERE name IN ('alerts', 'alert_escalations');
CREATE TABLE alert_source_escalations AS SELECT * FROM alert_escalations;
CREATE TABLE alerts_replacement (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id        INTEGER NOT NULL,
    probe_id TEXT NOT NULL DEFAULT 'local' CHECK (length(probe_id) > 0),
    assignment_generation INTEGER NOT NULL DEFAULT 1 CHECK (typeof(assignment_generation) = 'integer' AND assignment_generation > 0),
    status            TEXT NOT NULL,
    message           TEXT NOT NULL,
    fired_at          TIMESTAMP NOT NULL,
    acked_at          TIMESTAMP NULL,
    acked_by_user_id  INTEGER NULL,
    resolved_at       TIMESTAMP NULL,
    ack_token         TEXT NOT NULL,
    open_monitor_id   INTEGER NULL,
    created_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (acked_by_user_id) REFERENCES users(id) ON DELETE SET NULL,
    UNIQUE (ack_token),
    UNIQUE (open_monitor_id, probe_id, assignment_generation)
);

INSERT INTO alerts_replacement (id, monitor_id, probe_id, assignment_generation, status, message, fired_at, acked_at, acked_by_user_id, resolved_at, ack_token, open_monitor_id, created_at, updated_at) SELECT id, monitor_id, probe_id, assignment_generation, status, message, fired_at, acked_at, acked_by_user_id, resolved_at, ack_token, open_monitor_id, created_at, updated_at FROM alerts;
UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT seq FROM alert_source_sequences WHERE name = 'alerts'), 0)) WHERE name = 'alerts_replacement';
INSERT INTO sqlite_sequence (name, seq) SELECT 'alerts_replacement', seq FROM alert_source_sequences WHERE name = 'alerts' AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'alerts_replacement');
DROP TABLE alert_escalations;
DROP TABLE alerts;
ALTER TABLE alerts_replacement RENAME TO alerts;
CREATE INDEX idx_alerts_monitor ON alerts (monitor_id);
CREATE INDEX idx_alerts_status ON alerts (status);
CREATE INDEX idx_alerts_fired ON alerts (fired_at);
CREATE TABLE alert_escalations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    alert_id    INTEGER NOT NULL,
    monitor_id  INTEGER NOT NULL,
    policy_id   INTEGER NOT NULL,
    next_step   INTEGER NOT NULL,
    next_run_at TIMESTAMP NOT NULL,
    status      TEXT NOT NULL,
    lease_owner TEXT NULL,
    lease_until TIMESTAMP NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (alert_id) REFERENCES alerts(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE (alert_id)
);

CREATE INDEX idx_alert_escalations_due ON alert_escalations (status, next_run_at);
INSERT INTO alert_escalations SELECT * FROM alert_source_escalations;
UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT seq FROM alert_source_sequences WHERE name = 'alert_escalations'), 0)) WHERE name = 'alert_escalations';
INSERT INTO sqlite_sequence (name, seq) SELECT name, seq FROM alert_source_sequences WHERE name = 'alert_escalations' AND NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name = 'alert_escalations');
DROP TABLE alert_source_escalations;
DROP TABLE alert_source_sequences;
