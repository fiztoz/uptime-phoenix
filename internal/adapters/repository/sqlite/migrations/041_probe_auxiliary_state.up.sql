-- Stop all writers. Legacy evidence belongs to local generation one only.
CREATE TABLE monitor_conditions_regional (
    monitor_id INTEGER NOT NULL,
    probe_id TEXT NOT NULL DEFAULT 'local' CHECK (length(probe_id) > 0),
    assignment_generation INTEGER NOT NULL DEFAULT 1 CHECK (typeof(assignment_generation) = 'integer' AND assignment_generation > 0),
    kind TEXT NOT NULL,
    state TEXT NOT NULL,
    used_value REAL NULL,
    limit_value REAL NULL,
    percent_value REAL NULL,
    threshold_value REAL NULL,
    unit TEXT NOT NULL DEFAULT '',
    resource TEXT NOT NULL DEFAULT '',
    scope TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT '',
    message TEXT NOT NULL,
    observed_at TIMESTAMP NOT NULL,
    stale_after TIMESTAMP NOT NULL,
    last_success_at TIMESTAMP NULL,
    consecutive_state TEXT NOT NULL DEFAULT '',
    consecutive_count INTEGER NOT NULL DEFAULT 0,
    last_notified_state TEXT NOT NULL DEFAULT '',
    last_notified_at TIMESTAMP NULL,
    PRIMARY KEY (monitor_id, probe_id, assignment_generation, kind),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

INSERT INTO monitor_conditions_regional (monitor_id, kind, state, used_value, limit_value, percent_value, threshold_value, unit, resource, scope, source, message, observed_at, stale_after, last_success_at, consecutive_state, consecutive_count, last_notified_state, last_notified_at) SELECT monitor_id, kind, state, used_value, limit_value, percent_value, threshold_value, unit, resource, scope, source, message, observed_at, stale_after, last_success_at, consecutive_state, consecutive_count, last_notified_state, last_notified_at FROM monitor_conditions;
DROP TABLE monitor_conditions;
ALTER TABLE monitor_conditions_regional RENAME TO monitor_conditions;
CREATE INDEX idx_monitor_conditions_state ON monitor_conditions(state, stale_after);
CREATE TABLE tls_info_regional (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL,
    probe_id TEXT NOT NULL DEFAULT 'local' CHECK (length(probe_id) > 0),
    assignment_generation INTEGER NOT NULL DEFAULT 1 CHECK (typeof(assignment_generation) = 'integer' AND assignment_generation > 0),
    info_json TEXT NOT NULL,
    checked_at TEXT NOT NULL DEFAULT (datetime('now')),
    UNIQUE (monitor_id, probe_id, assignment_generation),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);
INSERT INTO tls_info_regional (id, monitor_id, info_json, checked_at)
SELECT id, monitor_id, info_json, checked_at FROM tls_info;
UPDATE sqlite_sequence SET seq = MAX(seq, COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'tls_info'), 0)) WHERE name = 'tls_info_regional';
DROP TABLE tls_info;
ALTER TABLE tls_info_regional RENAME TO tls_info;
