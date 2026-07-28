-- F2.3: escalation policies. See the MariaDB copy of this migration and
-- docs/F2.3-ESCALATION-CONTRACTS.md for the full rationale.
--
-- Assignment tables carry a UNIQUE on the entity column (not on the pair), so a
-- monitor or group has AT MOST ONE policy.

CREATE TABLE escalation_policies (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL,
    name        TEXT NOT NULL,
    description TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT 1,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE INDEX idx_escalation_policies_user ON escalation_policies (user_id);

CREATE TABLE escalation_steps (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    policy_id    INTEGER NOT NULL,
    step_order   INTEGER NOT NULL,
    wait_minutes INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE (policy_id, step_order)
);

CREATE TABLE escalation_step_notifications (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    step_id         INTEGER NOT NULL,
    notification_id INTEGER NOT NULL,
    FOREIGN KEY (step_id) REFERENCES escalation_steps(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    UNIQUE (step_id, notification_id)
);

CREATE TABLE escalation_policy_monitors (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    monitor_id INTEGER NOT NULL,
    policy_id  INTEGER NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE (monitor_id)
);

CREATE INDEX idx_escalation_policy_monitors_policy ON escalation_policy_monitors (policy_id);

CREATE TABLE escalation_policy_groups (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    group_id  INTEGER NOT NULL,
    policy_id INTEGER NOT NULL,
    FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE (group_id)
);

CREATE INDEX idx_escalation_policy_groups_policy ON escalation_policy_groups (policy_id);

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
