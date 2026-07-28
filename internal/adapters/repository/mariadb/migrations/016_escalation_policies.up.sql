-- F2.3: escalation policies.
--
-- A policy is an ordered ladder of steps; each step names notification channels
-- and a wait duration measured from the PREVIOUS step. Steps run only while the
-- alert is still firing. See docs/F2.3-ESCALATION-CONTRACTS.md.
--
-- Assignment tables carry a UNIQUE on the entity column (not on the pair), so a
-- monitor or group has AT MOST ONE policy. That is the schema enforcing
-- contract 1's "exactly one policy escalates a given alert" rather than
-- application code trying to.

CREATE TABLE escalation_policies (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    user_id     BIGINT NOT NULL,
    name        VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE,
    INDEX idx_escalation_policies_user (user_id)
);

CREATE TABLE escalation_steps (
    id           BIGINT AUTO_INCREMENT PRIMARY KEY,
    policy_id    BIGINT NOT NULL,
    step_order   INT NOT NULL,
    wait_minutes INT NOT NULL DEFAULT 0,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE KEY uq_escalation_steps_order (policy_id, step_order)
);

CREATE TABLE escalation_step_notifications (
    id              BIGINT AUTO_INCREMENT PRIMARY KEY,
    step_id         BIGINT NOT NULL,
    notification_id BIGINT NOT NULL,
    FOREIGN KEY (step_id) REFERENCES escalation_steps(id) ON DELETE CASCADE,
    FOREIGN KEY (notification_id) REFERENCES notifications(id) ON DELETE CASCADE,
    UNIQUE KEY uq_escalation_step_notif (step_id, notification_id)
);

CREATE TABLE escalation_policy_monitors (
    id         BIGINT AUTO_INCREMENT PRIMARY KEY,
    monitor_id BIGINT NOT NULL,
    policy_id  BIGINT NOT NULL,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE KEY uq_escalation_policy_monitor (monitor_id),
    INDEX idx_escalation_policy_monitors_policy (policy_id)
);

CREATE TABLE escalation_policy_groups (
    id        BIGINT AUTO_INCREMENT PRIMARY KEY,
    group_id  BIGINT NOT NULL,
    policy_id BIGINT NOT NULL,
    FOREIGN KEY (group_id) REFERENCES monitor_groups(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE KEY uq_escalation_policy_group (group_id),
    INDEX idx_escalation_policy_groups_policy (policy_id)
);

-- One row per alert. next_run_at is the scheduling clock, so progress survives a
-- restart. lease_owner/lease_until are the compare-and-set claim that stops two
-- sharded workers from sending the same step twice.
CREATE TABLE alert_escalations (
    id          BIGINT AUTO_INCREMENT PRIMARY KEY,
    alert_id    BIGINT NOT NULL,
    monitor_id  BIGINT NOT NULL,
    policy_id   BIGINT NOT NULL,
    next_step   INT NOT NULL,
    next_run_at TIMESTAMP NOT NULL,
    status      VARCHAR(20) NOT NULL,
    lease_owner VARCHAR(255) NULL DEFAULT NULL,
    lease_until TIMESTAMP NULL DEFAULT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    FOREIGN KEY (alert_id) REFERENCES alerts(id) ON DELETE CASCADE,
    FOREIGN KEY (monitor_id) REFERENCES monitors(id) ON DELETE CASCADE,
    FOREIGN KEY (policy_id) REFERENCES escalation_policies(id) ON DELETE CASCADE,
    UNIQUE KEY uq_alert_escalations_alert (alert_id),
    INDEX idx_alert_escalations_due (status, next_run_at)
);
