CREATE TABLE monitor_conditions (
    monitor_id INTEGER NOT NULL,
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
    PRIMARY KEY (monitor_id, kind),
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE
);

CREATE INDEX idx_monitor_conditions_state
    ON monitor_conditions(state, stale_after);
